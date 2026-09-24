// tisch_download.go
//
// Downloader for the TISCH2 single-cell RNA-seq database gallery
// (https://tisch.compbio.cn/gallery/).
//
// It scrapes the gallery page for the list of dataset IDs, then downloads the
// per-dataset data files that the website exposes under:
//
//	/static/data/<ID>/<ID>_expression.h5           (h5     : expression matrix, HDF5)
//	/static/data/<ID>/<ID>_Expression.zip          (exprzip: 10x-style mtx bundle, zipped)
//	/static/data/<ID>/<ID>_CellMetainfo_table.tsv  (meta   : per-cell metadata)
//	/static/data/<ID>/<ID>_AllDiffGenes_table.tsv  (de     : differential-expression table)
//
// Files are saved into <out>/<ID>/. Downloads are resumable: a file is skipped
// when its local size already matches the server's Content-Length. Missing
// files (HTTP 404) are reported and skipped so one absent file never aborts a run.
//
// The TISCH server publishes an unreachable IPv6 record on some networks, so by
// default the tool dials over IPv4 only. Use -ipv4=false to allow IPv6.
//
// Usage examples:
//
//	go run tisch_download.go -list
//	go run tisch_download.go -out tisch_data
//	go run tisch_download.go -types h5,meta -concurrency 6
//	go run tisch_download.go -datasets AEL_GSE142213,UVM_GSE139829
//
// Build a standalone binary:
//
//	go build -o tisch_download tisch_download.go
package main

import (
	"context"
	"crypto/tls"
	"flag"
	"fmt"
	"html"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

// fileType describes one downloadable artifact per dataset.
type fileType struct {
	key    string // short selector used on the -types flag
	suffix string // filename suffix appended to the dataset ID
	desc   string
}

// allTypes lists every artifact the gallery exposes per dataset, in the order
// they are downloaded.
var allTypes = []fileType{
	{"h5", "_expression.h5", "expression matrix (HDF5)"},
	{"exprzip", "_Expression.zip", "expression matrix (zipped mtx)"},
	{"meta", "_CellMetainfo_table.tsv", "per-cell metadata (TSV)"},
	{"de", "_AllDiffGenes_table.tsv", "differential-expression table (TSV)"},
}

// checkboxRe extracts dataset IDs from the gallery page's selection checkboxes,
// e.g. value="AEL_GSE142213" name="dataset_checkbox_list".
var checkboxRe = regexp.MustCompile(`value="([^"]+)"\s+name="dataset_checkbox_list"`)

type config struct {
	base        string
	out         string
	concurrency int
	types       []fileType
	datasets    []string // explicit subset; empty means "all from gallery"
	retries     int
	timeout     time.Duration
	listOnly    bool
	infoOnly    bool
}

func main() {
	var (
		base        = flag.String("base", "https://tisch.compbio.cn", "Base URL of the TISCH site")
		out         = flag.String("out", "tisch_data", "Output directory")
		concurrency = flag.Int("concurrency", 4, "Number of datasets downloaded in parallel")
		typesFlag   = flag.String("types", "h5,exprzip,meta,de", "Comma-separated artifact types: h5,exprzip,meta,de")
		datasets    = flag.String("datasets", "", "Comma-separated dataset IDs to fetch (default: all in gallery)")
		retries     = flag.Int("retries", 3, "Retry attempts per file")
		timeout     = flag.Duration("timeout", 30*time.Minute, "Per-file download timeout")
		forceIPv4   = flag.Bool("ipv4", true, "Dial over IPv4 only (TISCH's IPv6 record is often unroutable)")
		listOnly    = flag.Bool("list", false, "List dataset IDs and exit")
		infoOnly    = flag.Bool("info", false, "Print dataset metadata (species, cancer, cells, platform, PMID) and exit")
	)
	flag.Parse()

	types, err := parseTypes(*typesFlag)
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(2)
	}

	cfg := config{
		base:        strings.TrimRight(*base, "/"),
		out:         *out,
		concurrency: max(1, *concurrency),
		types:       types,
		datasets:    splitCSV(*datasets),
		retries:     max(1, *retries),
		timeout:     *timeout,
		listOnly:    *listOnly,
		infoOnly:    *infoOnly,
	}

	client := newClient(*forceIPv4, cfg.timeout)

	ids := cfg.datasets
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "Fetching gallery dataset list...")
		ids, err = fetchDatasetIDs(client, cfg.base)
		if err != nil {
			fmt.Fprintln(os.Stderr, "error: could not fetch gallery:", err)
			os.Exit(1)
		}
	}
	sort.Strings(ids)
	fmt.Fprintf(os.Stderr, "Found %d dataset(s).\n", len(ids))

	if cfg.listOnly {
		for _, id := range ids {
			fmt.Println(id)
		}
		return
	}

	if cfg.infoOnly {
		if err := printCatalog(client, cfg, ids); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}

	if err := os.MkdirAll(cfg.out, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "error: cannot create output dir:", err)
		os.Exit(1)
	}

	run(client, cfg, ids)
}

// run downloads all requested files for the given datasets using a worker pool.
func run(client *http.Client, cfg config, ids []string) {
	type job struct{ id string }
	jobs := make(chan job)

	var (
		wg               sync.WaitGroup
		mu               sync.Mutex
		okCount, skipCnt int
		missCount, errCnt int
	)

	worker := func() {
		defer wg.Done()
		for j := range jobs {
			dir := filepath.Join(cfg.out, j.id)
			if err := os.MkdirAll(dir, 0o755); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] cannot create dir: %v\n", j.id, err)
				mu.Lock()
				errCnt++
				mu.Unlock()
				continue
			}
			for _, ft := range cfg.types {
				name := j.id + ft.suffix
				url := fmt.Sprintf("%s/static/data/%s/%s", cfg.base, j.id, name)
				dest := filepath.Join(dir, name)

				status, err := downloadFile(client, url, dest, cfg.retries)
				mu.Lock()
				switch status {
				case statusDownloaded:
					okCount++
					fmt.Printf("[ok]   %s\n", filepath.Join(j.id, name))
				case statusSkipped:
					skipCnt++
					fmt.Printf("[skip] %s (already complete)\n", filepath.Join(j.id, name))
				case statusMissing:
					missCount++
					fmt.Printf("[miss] %s (404 - not published)\n", filepath.Join(j.id, name))
				default:
					errCnt++
					fmt.Fprintf(os.Stderr, "[err]  %s: %v\n", filepath.Join(j.id, name), err)
				}
				mu.Unlock()
			}
		}
	}

	for i := 0; i < cfg.concurrency; i++ {
		wg.Add(1)
		go worker()
	}
	for _, id := range ids {
		jobs <- job{id}
	}
	close(jobs)
	wg.Wait()

	fmt.Fprintf(os.Stderr, "\nDone. downloaded=%d skipped=%d missing=%d errors=%d\n",
		okCount, skipCnt, missCount, errCnt)
	if errCnt > 0 {
		os.Exit(1)
	}
}

type dlStatus int

const (
	statusError dlStatus = iota
	statusDownloaded
	statusSkipped
	statusMissing
)

// downloadFile fetches url into dest with resume/skip support and retries.
func downloadFile(client *http.Client, url, dest string, retries int) (dlStatus, error) {
	var lastErr error
	for attempt := 1; attempt <= retries; attempt++ {
		st, err := downloadOnce(client, url, dest)
		if err == nil {
			return st, nil
		}
		lastErr = err
		if st == statusMissing {
			return statusMissing, nil
		}
		if attempt < retries {
			time.Sleep(time.Duration(attempt) * 2 * time.Second)
		}
	}
	return statusError, lastErr
}

func downloadOnce(client *http.Client, url, dest string) (dlStatus, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return statusError, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (tisch_download.go)")

	resp, err := client.Do(req)
	if err != nil {
		return statusError, err
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return statusMissing, fmt.Errorf("404 not found")
	}
	if resp.StatusCode != http.StatusOK {
		return statusError, fmt.Errorf("unexpected status %s", resp.Status)
	}

	// Skip if the local file already matches the remote size.
	if resp.ContentLength > 0 {
		if fi, statErr := os.Stat(dest); statErr == nil && fi.Size() == resp.ContentLength {
			return statusSkipped, nil
		}
	}

	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return statusError, err
	}
	written, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		os.Remove(tmp)
		return statusError, copyErr
	}
	if closeErr != nil {
		os.Remove(tmp)
		return statusError, closeErr
	}
	if resp.ContentLength > 0 && written != resp.ContentLength {
		os.Remove(tmp)
		return statusError, fmt.Errorf("short read: got %d of %d bytes", written, resp.ContentLength)
	}
	if err := os.Rename(tmp, dest); err != nil {
		return statusError, err
	}
	return statusDownloaded, nil
}

// datasetInfo holds the per-dataset metadata scraped from the gallery table.
type datasetInfo struct {
	id        string
	tid       string // internal TISCH id, e.g. T010001
	species   string
	treatment string
	patients  string
	cells     string
	platform  string
	priMeta   string // Primary / Metastatic
	pmid      string
	citation  string
}

// cancerName maps a dataset ID prefix (cancer-type abbreviation) to its full
// name. Mostly follows TCGA codes. Prefixes not listed fall back to the raw
// abbreviation. A few abbreviations are ambiguous across sources; verify on the
// dataset's own page or its linked publication if precision matters.
var cancerName = map[string]string{
	"AEL":          "Acute Erythroid Leukemia",
	"ALL":          "Acute Lymphoblastic Leukemia",
	"AML":          "Acute Myeloid Leukemia",
	"BCC":          "Basal Cell Carcinoma",
	"BLCA":         "Bladder Urothelial Carcinoma",
	"BRCA":         "Breast Cancer",
	"CESC":         "Cervical Squamous Cell Carcinoma",
	"CHOL":         "Cholangiocarcinoma",
	"CLL":          "Chronic Lymphocytic Leukemia",
	"CRC":          "Colorectal Cancer",
	"DLBC":         "Diffuse Large B-cell Lymphoma",
	"ESCA":         "Esophageal Carcinoma",
	"GCTB":         "Giant Cell Tumor of Bone",
	"GIST":         "Gastrointestinal Stromal Tumor",
	"Glioma":       "Glioma",
	"HB":           "Hepatoblastoma",
	"HNSC":         "Head and Neck Squamous Cell Carcinoma",
	"KICH":         "Kidney Chromophobe",
	"KIPAN":        "Pan-Kidney (KICH+KIRC+KIRP)",
	"KIRC":         "Kidney Renal Clear Cell Carcinoma",
	"LIHC":         "Liver Hepatocellular Carcinoma",
	"LSCC":         "Laryngeal Squamous Cell Carcinoma",
	"MB":           "Medulloblastoma",
	"MCC":          "Merkel Cell Carcinoma",
	"MF":           "Mycosis Fungoides",
	"MM":           "Multiple Myeloma",
	"MPNST":        "Malignant Peripheral Nerve Sheath Tumor",
	"NB":           "Neuroblastoma",
	"NET":          "Neuroendocrine Tumor",
	"Neurofibroma": "Neurofibroma",
	"NHL":          "Non-Hodgkin Lymphoma",
	"NPC":          "Nasopharyngeal Carcinoma",
	"NSCLC":        "Non-Small-Cell Lung Cancer",
	"OS":           "Osteosarcoma",
	"OSCC":         "Oral Squamous Cell Carcinoma",
	"OV":           "Ovarian Cancer",
	"PAAD":         "Pancreatic Adenocarcinoma",
	"PBMC":         "Peripheral Blood Mononuclear Cells",
	"PCFCL":        "Primary Cutaneous Follicle Center Lymphoma",
	"PPB":          "Pleuropulmonary Blastoma",
	"PRAD":         "Prostate Adenocarcinoma",
	"RB":           "Retinoblastoma",
	"SARC":         "Sarcoma",
	"SCC":          "Squamous Cell Carcinoma",
	"SCLC":         "Small-Cell Lung Cancer",
	"SKCM":         "Skin Cutaneous Melanoma",
	"SS":           "Synovial Sarcoma",
	"STAD":         "Stomach Adenocarcinoma",
	"THCA":         "Thyroid Carcinoma",
	"UCEC":         "Uterine Corpus Endometrial Carcinoma",
	"UVM":          "Uveal Melanoma",
}

// cancerFromID returns the decoded cancer type for a dataset ID, based on the
// prefix before the first underscore.
func cancerFromID(id string) string {
	prefix := id
	if i := strings.IndexByte(id, '_'); i >= 0 {
		prefix = id[:i]
	}
	if name, ok := cancerName[prefix]; ok {
		return name
	}
	return prefix
}

var (
	trBlockRe    = regexp.MustCompile(`(?s)<tr>(.*?)</tr>`)
	commentRe    = regexp.MustCompile(`(?s)<!--.*?-->`)
	tdRe         = regexp.MustCompile(`(?s)<td[^>]*>(.*?)</td>`)
	tagRe        = regexp.MustCompile(`(?s)<[^>]*>`)
	pmidTitleRe  = regexp.MustCompile(`<td\s+title="([^"]*)"[^>]*class="checkbox-td"`)
	wsRe         = regexp.MustCompile(`\s+`)
)

// parseCatalog extracts per-dataset metadata from the gallery HTML.
func parseCatalog(body string) map[string]datasetInfo {
	out := map[string]datasetInfo{}
	for _, block := range trBlockRe.FindAllStringSubmatch(body, -1) {
		row := block[1]
		m := checkboxRe.FindStringSubmatch(row)
		if m == nil {
			continue // not a dataset row
		}
		info := datasetInfo{id: m[1]}

		// Citation lives in the PMID cell's title attribute (before comment strip).
		if c := pmidTitleRe.FindStringSubmatch(row); c != nil {
			info.citation = cleanText(c[1])
		}

		// Remove commented-out cells, then read the visible <td> texts in order.
		clean := commentRe.ReplaceAllString(row, "")
		var cells []string
		for _, td := range tdRe.FindAllStringSubmatch(clean, -1) {
			cells = append(cells, cleanText(td[1]))
		}
		// Expected order: [0]=checkbox(empty) [1]=T-id [2]=Name [3]=Species
		// [4]=Treatment [5]=Patients [6]=Cells [7]=Platform [8]=Pri/Meta [9]=PMID
		get := func(i int) string {
			if i < len(cells) {
				return cells[i]
			}
			return ""
		}
		info.tid = get(1)
		info.species = get(3)
		info.treatment = get(4)
		info.patients = get(5)
		info.cells = get(6)
		info.platform = get(7)
		info.priMeta = get(8)
		info.pmid = get(9)

		out[info.id] = info
	}
	return out
}

// cleanText strips HTML tags, unescapes entities, and collapses whitespace.
func cleanText(s string) string {
	s = tagRe.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = wsRe.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// printCatalog fetches the gallery and prints a metadata table for the given IDs.
func printCatalog(client *http.Client, cfg config, ids []string) error {
	req, err := http.NewRequest(http.MethodGet, cfg.base+"/gallery/", nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (tisch_download.go)")
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("gallery returned %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}

	catalog := parseCatalog(string(body))

	w := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "DATASET_ID\tCANCER\tSPECIES\tTREATMENT\tPATIENTS\tCELLS\tPLATFORM\tPRI/META\tPMID")
	for _, id := range ids {
		info := catalog[id]
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			id, cancerFromID(id), dash(info.species), dash(info.treatment),
			dash(info.patients), dash(info.cells), dash(info.platform),
			dash(info.priMeta), dash(info.pmid))
	}
	if err := w.Flush(); err != nil {
		return err
	}

	// Print citations separately (too long for the aligned table).
	fmt.Println()
	for _, id := range ids {
		if c := catalog[id].citation; c != "" {
			fmt.Printf("%s: %s\n", id, c)
		}
	}
	return nil
}

func dash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

// fetchDatasetIDs downloads the gallery page and extracts unique dataset IDs.
func fetchDatasetIDs(client *http.Client, base string) ([]string, error) {
	req, err := http.NewRequest(http.MethodGet, base+"/gallery/", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (tisch_download.go)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("gallery returned %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	seen := map[string]bool{}
	var ids []string
	for _, m := range checkboxRe.FindAllStringSubmatch(string(body), -1) {
		id := m[1]
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, fmt.Errorf("no dataset IDs found (page layout may have changed)")
	}
	return ids, nil
}

// newClient builds an HTTP client, optionally pinned to IPv4 dialing.
func newClient(forceIPv4 bool, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   30 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if forceIPv4 {
		tr.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return dialer.DialContext(ctx, "tcp4", addr)
		}
	} else {
		tr.DialContext = dialer.DialContext
	}
	return &http.Client{Transport: tr, Timeout: timeout}
}

func parseTypes(s string) ([]fileType, error) {
	want := splitCSV(s)
	if len(want) == 0 {
		return allTypes, nil
	}
	byKey := map[string]fileType{}
	for _, ft := range allTypes {
		byKey[ft.key] = ft
	}
	var out []fileType
	for _, w := range want {
		ft, ok := byKey[strings.ToLower(w)]
		if !ok {
			return nil, fmt.Errorf("unknown type %q (valid: h5, exprzip, meta, de)", w)
		}
		out = append(out, ft)
	}
	return out, nil
}

func splitCSV(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}
