# TISCH2 Downloader

A small, dependency-free Go tool for bulk-downloading single-cell RNA-seq data
from the **TISCH2** gallery (Tumor Immune Single-cell Hub 2,
<https://tisch.compbio.cn/gallery/>).

It scrapes the gallery for the full list of dataset IDs and downloads the
per-dataset data files that the site publishes, with parallel workers, retries,
resume support, and a metadata browser.

---

## What it downloads

TISCH2 serves the following files for each dataset under
`/static/data/<ID>/`. The tool can fetch any subset of them:

| Type key  | File suffix                    | Description                          |
|-----------|--------------------------------|--------------------------------------|
| `h5`      | `<ID>_expression.h5`           | Expression matrix (HDF5)             |
| `exprzip` | `<ID>_Expression.zip`          | Expression matrix (zipped 10x mtx)   |
| `meta`    | `<ID>_CellMetainfo_table.tsv`  | Per-cell metadata (TSV)              |
| `de`      | `<ID>_AllDiffGenes_table.tsv`  | Differential-expression table (TSV)  |

Files are saved to `<out>/<ID>/`, e.g. `tisch_data/AEL_GSE142213/AEL_GSE142213_expression.h5`.

---

## Understanding dataset IDs

Dataset IDs follow the pattern `<CancerType>_<Accession>[_suffixes]`:

- **`AEL`** — cancer-type abbreviation (prefix, before the first underscore), mostly TCGA codes
- **`GSE142213`** — the source data accession (usually an NCBI GEO series; sometimes `EMTAB…`, `SRP…`)
- optional suffixes:
  - **platform**: `_10X`, `_Smartseq2`, `_inDrop`
  - **species**: `_mouse`
  - **treatment**: `_aPD1`, `_aPDL1`, `_aCTLA4`, `_aTIM3`

Example: `CRC_GSE146771_Smartseq2` → Colorectal Cancer, GEO GSE146771, sequenced with Smart-seq2.

Use `-info` (see below) to print the full decoded metadata for any dataset.

---

## Requirements

- Go 1.21 or newer (uses the standard library only — no external modules)

---

## Build

```bash
go build -o tisch_download tisch_download.go
```

Or run without building:

```bash
go run tisch_download.go [flags]
```

---

## Usage

```
tisch_download [flags]
```

### Flags

| Flag           | Default                     | Description                                                              |
|----------------|-----------------------------|--------------------------------------------------------------------------|
| `-out`         | `tisch_data`                | Output directory                                                         |
| `-datasets`    | *(all)*                     | Comma-separated dataset IDs to fetch (default: every dataset in gallery) |
| `-types`       | `h5,exprzip,meta,de`        | Comma-separated artifact types to download                               |
| `-concurrency` | `4`                         | Number of datasets downloaded in parallel                                |
| `-retries`     | `3`                         | Retry attempts per file                                                  |
| `-timeout`     | `30m`                       | Per-file download timeout                                                |
| `-ipv4`        | `true`                      | Dial over IPv4 only (TISCH's IPv6 record is often unroutable)            |
| `-base`        | `https://tisch.compbio.cn`  | Base URL of the TISCH site                                               |
| `-list`        | `false`                     | List dataset IDs and exit                                                |
| `-info`        | `false`                     | Print dataset metadata table and exit                                    |

---

## Examples

List every dataset ID:

```bash
./tisch_download -list
```

Find datasets by cancer type:

```bash
./tisch_download -list | grep -i brca
```

Show metadata (cancer, species, treatment, patients, cells, platform, PMID, citation):

```bash
./tisch_download -info -datasets AEL_GSE142213,BRCA_GSE176078
./tisch_download -info                       # all datasets
./tisch_download -info | grep -i melanoma    # filter by cancer type
```

Example `-info` output:

```
DATASET_ID      CANCER                    SPECIES  TREATMENT  PATIENTS  CELLS   PLATFORM      PRI/META  PMID
AEL_GSE142213   Acute Erythroid Leukemia  Human    None       2         3,994   10x Genomics  Primary   32330454
BRCA_GSE176078  Breast Cancer             Human    None       26        89,471  10x Genomics  Primary   34493872

AEL_GSE142213: Di Genua C, et al. Cancer Cell 2020
BRCA_GSE176078: Wu SZ, et al. Nat Genet 2021
```

Download everything (all datasets, all four file types):

```bash
./tisch_download -out tisch_data
```

Download a single dataset:

```bash
./tisch_download -datasets AEL_GSE142213
```

Download several datasets, only the metadata and DE tables (skips large matrices):

```bash
./tisch_download -datasets AEL_GSE142213,UVM_GSE139829 -types meta,de
```

Download only expression matrices with more parallelism:

```bash
./tisch_download -types h5 -concurrency 6
```

---

## Behavior notes

- **Resumable / idempotent.** Before downloading, the tool compares the local
  file size to the server's `Content-Length` and skips files that are already
  complete. Re-running is safe and only fetches what's missing. Downloads are
  written to a `.part` file and renamed on success, so interrupted runs never
  leave a truncated file in place.
- **Missing files are tolerated.** Not every dataset publishes every artifact
  (e.g. some mouse datasets). A `404` is reported as `[miss]` and the run
  continues.
- **IPv4 by default.** TISCH's DNS sometimes returns an IPv6 address that is not
  routable on many networks, which makes `curl`/browsers hang. The tool dials
  IPv4 by default; pass `-ipv4=false` to allow IPv6.
- **Status output.** Each file prints one of `[ok]`, `[skip]`, `[miss]`, or
  `[err]`, followed by a final summary line:
  `downloaded=… skipped=… missing=… errors=…`. The process exits non-zero if any
  file errored (after retries).

---

## Data volume warning

Downloading **all 190 datasets** with the `h5` and `exprzip` matrices is many
gigabytes. Start small:

- Use `-types meta,de` to grab just the small TSV tables, or
- Use `-datasets <ID>` to pull one dataset first,

then scale up once you've confirmed it does what you need.

---

## How it works

1. `GET /gallery/` and parse dataset IDs from the selection checkboxes
   (`value="<ID>" name="dataset_checkbox_list"`).
2. For each dataset, request the selected files from
   `/static/data/<ID>/<ID><suffix>`.
3. A worker pool (`-concurrency`) downloads datasets in parallel; each file is
   fetched with retries, resume/skip logic, and atomic `.part` → final rename.

The `-info` command additionally parses the gallery table for each dataset's
species, treatment, patient/cell counts, platform, primary/metastatic status,
PMID, and citation. Cancer full-names are decoded from the ID prefix via a
built-in map (mostly TCGA codes); the remaining columns are scraped live and are
authoritative.

---

## Attribution & terms

TISCH2 is a resource from the Wang Lab. Please review the site's terms of use and
cite the TISCH2 publication when using downloaded data:

> Han Y, Wang Y, Dong X, *et al.* **TISCH2: expanded datasets and new tools for
> single-cell transcriptome analyses of the tumor microenvironment.**
> *Nucleic Acids Research*, 2023. <https://doi.org/10.1093/nar/gkac959>

Be considerate with `-concurrency` to avoid overloading the server. This tool is
an unofficial community helper and is not affiliated with TISCH2.
