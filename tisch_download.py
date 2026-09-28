#!/usr/bin/env python3
"""
TISCH2 downloader (Python / Selenium edition).

Scrapes the TISCH2 gallery (https://tisch.compbio.cn/gallery/) for dataset IDs
and per-dataset metadata using Selenium, then downloads the published data files
with requests (parallel workers, retries, resume/skip, atomic writes).

Files fetched per dataset (choose with --types):

    h5      -> <ID>_expression.h5           expression matrix (HDF5)
    exprzip -> <ID>_Expression.zip          expression matrix (zipped 10x mtx)
    meta    -> <ID>_CellMetainfo_table.tsv  per-cell metadata (TSV)
    de      -> <ID>_AllDiffGenes_table.tsv  differential-expression table (TSV)

They are saved to <out>/<ID>/.

Why Selenium is optional: the gallery is server-rendered, so the dataset list
lives in the static HTML. Selenium is used here (as requested) to read the
rendered DOM, which conveniently hides the page's commented-out table cells and
makes metadata parsing straightforward. Selenium is only started when scraping
is actually needed (--list, --info, or a download without an explicit
--datasets list). Targeted downloads with --datasets need only requests.

Requirements:
    pip install selenium requests
    # Selenium 4.6+ auto-manages the browser driver (Selenium Manager).
    # A Chrome/Chromium install is required for the default browser.

Examples:
    python tisch_download.py --list
    python tisch_download.py --info --datasets AEL_GSE142213,BRCA_GSE176078
    python tisch_download.py --datasets AEL_GSE142213 --types meta,de
    python tisch_download.py --out tisch_data --concurrency 6
"""

from __future__ import annotations

import argparse
import os
import socket
import sys
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from threading import Lock

import requests

# --------------------------------------------------------------------------- #
# Static configuration
# --------------------------------------------------------------------------- #

DEFAULT_BASE = "https://tisch.compbio.cn"

# type key -> (filename suffix, human description)
FILE_TYPES = {
    "h5":      ("_expression.h5",           "expression matrix (HDF5)"),
    "exprzip": ("_Expression.zip",          "expression matrix (zipped mtx)"),
    "meta":    ("_CellMetainfo_table.tsv",  "per-cell metadata (TSV)"),
    "de":      ("_AllDiffGenes_table.tsv",  "differential-expression table (TSV)"),
}

USER_AGENT = "tisch_download.py (+https://tisch.compbio.cn)"

# Cancer-type abbreviations (dataset ID prefix). Mostly TCGA codes. Prefixes not
# listed fall back to the raw abbreviation. A few (LSCC, SS, MF) are ambiguous
# across sources; trust the dataset's linked PMID/citation if precision matters.
CANCER_NAME = {
    "AEL": "Acute Erythroid Leukemia",
    "ALL": "Acute Lymphoblastic Leukemia",
    "AML": "Acute Myeloid Leukemia",
    "BCC": "Basal Cell Carcinoma",
    "BLCA": "Bladder Urothelial Carcinoma",
    "BRCA": "Breast Cancer",
    "CESC": "Cervical Squamous Cell Carcinoma",
    "CHOL": "Cholangiocarcinoma",
    "CLL": "Chronic Lymphocytic Leukemia",
    "CRC": "Colorectal Cancer",
    "DLBC": "Diffuse Large B-cell Lymphoma",
    "ESCA": "Esophageal Carcinoma",
    "GCTB": "Giant Cell Tumor of Bone",
    "GIST": "Gastrointestinal Stromal Tumor",
    "Glioma": "Glioma",
    "HB": "Hepatoblastoma",
    "HNSC": "Head and Neck Squamous Cell Carcinoma",
    "KICH": "Kidney Chromophobe",
    "KIPAN": "Pan-Kidney (KICH+KIRC+KIRP)",
    "KIRC": "Kidney Renal Clear Cell Carcinoma",
    "LIHC": "Liver Hepatocellular Carcinoma",
    "LSCC": "Laryngeal Squamous Cell Carcinoma",
    "MB": "Medulloblastoma",
    "MCC": "Merkel Cell Carcinoma",
    "MF": "Mycosis Fungoides",
    "MM": "Multiple Myeloma",
    "MPNST": "Malignant Peripheral Nerve Sheath Tumor",
    "NB": "Neuroblastoma",
    "NET": "Neuroendocrine Tumor",
    "Neurofibroma": "Neurofibroma",
    "NHL": "Non-Hodgkin Lymphoma",
    "NPC": "Nasopharyngeal Carcinoma",
    "NSCLC": "Non-Small-Cell Lung Cancer",
    "OS": "Osteosarcoma",
    "OSCC": "Oral Squamous Cell Carcinoma",
    "OV": "Ovarian Cancer",
    "PAAD": "Pancreatic Adenocarcinoma",
    "PBMC": "Peripheral Blood Mononuclear Cells",
    "PCFCL": "Primary Cutaneous Follicle Center Lymphoma",
    "PPB": "Pleuropulmonary Blastoma",
    "PRAD": "Prostate Adenocarcinoma",
    "RB": "Retinoblastoma",
    "SARC": "Sarcoma",
    "SCC": "Squamous Cell Carcinoma",
    "SCLC": "Small-Cell Lung Cancer",
    "SKCM": "Skin Cutaneous Melanoma",
    "SS": "Synovial Sarcoma",
    "STAD": "Stomach Adenocarcinoma",
    "THCA": "Thyroid Carcinoma",
    "UCEC": "Uterine Corpus Endometrial Carcinoma",
    "UVM": "Uveal Melanoma",
}


def cancer_from_id(dataset_id: str) -> str:
    """Decode the cancer type from a dataset ID prefix (before first '_')."""
    prefix = dataset_id.split("_", 1)[0]
    return CANCER_NAME.get(prefix, prefix)


# --------------------------------------------------------------------------- #
# IPv4 forcing (TISCH's IPv6 record is often unroutable)
# --------------------------------------------------------------------------- #

def force_ipv4() -> None:
    """Make urllib3/requests resolve to IPv4 addresses only."""
    import urllib3.util.connection as urllib3_conn

    urllib3_conn.allowed_gai_family = lambda: socket.AF_INET


# --------------------------------------------------------------------------- #
# Selenium scraping
# --------------------------------------------------------------------------- #

def make_driver(headless: bool):
    """Create a Chrome WebDriver. Requires selenium and a Chrome/Chromium install."""
    try:
        from selenium import webdriver
        from selenium.webdriver.chrome.options import Options
    except ImportError as exc:  # pragma: no cover
        raise SystemExit(
            "Selenium is required for scraping (--list/--info or downloading all "
            "datasets). Install it with:  pip install selenium\n"
            f"(import error: {exc})"
        )

    opts = Options()
    if headless:
        opts.add_argument("--headless=new")
    opts.add_argument("--no-sandbox")
    opts.add_argument("--disable-dev-shm-usage")
    opts.add_argument("--disable-gpu")
    opts.add_argument("--window-size=1400,1000")
    opts.add_argument(f"--user-agent={USER_AGENT}")
    return webdriver.Chrome(options=opts)


def scrape_gallery(base: str, headless: bool):
    """
    Load the gallery once and return (ids, catalog).

    ids     : sorted list of dataset IDs
    catalog : dict id -> metadata dict (species, treatment, patients, cells,
              platform, pri_meta, pmid, citation)
    """
    from selenium.webdriver.common.by import By
    from selenium.webdriver.support import expected_conditions as EC
    from selenium.webdriver.support.ui import WebDriverWait

    driver = make_driver(headless)
    try:
        driver.get(base.rstrip("/") + "/gallery/")
        WebDriverWait(driver, 40).until(
            EC.presence_of_all_elements_located(
                (By.CSS_SELECTOR, 'input[name="dataset_checkbox_list"]')
            )
        )

        catalog: dict[str, dict] = {}
        rows = driver.find_elements(By.CSS_SELECTOR, "#dataset-table tbody tr")
        for row in rows:
            checks = row.find_elements(
                By.CSS_SELECTOR, 'input[name="dataset_checkbox_list"]'
            )
            if not checks:
                continue
            dataset_id = checks[0].get_attribute("value")
            if not dataset_id:
                continue

            # Only real (non-commented) <td> cells exist in the rendered DOM.
            # Order: [0] checkbox [1] T-id [2] Name [3] Species [4] Treatment
            #        [5] Patients [6] Cells [7] Platform [8] Pri/Meta [9] PMID
            tds = row.find_elements(By.TAG_NAME, "td")

            def cell(i: int) -> str:
                return tds[i].text.strip() if i < len(tds) else ""

            citation = ""
            if len(tds) >= 10:
                citation = (tds[9].get_attribute("title") or "").strip()

            catalog[dataset_id] = {
                "species": cell(3),
                "treatment": cell(4),
                "patients": cell(5),
                "cells": cell(6),
                "platform": cell(7),
                "pri_meta": cell(8),
                "pmid": cell(9),
                "citation": citation,
            }

        ids = sorted(catalog.keys())
        return ids, catalog
    finally:
        driver.quit()


# --------------------------------------------------------------------------- #
# Downloading (requests)
# --------------------------------------------------------------------------- #

def make_session(retries: int) -> requests.Session:
    sess = requests.Session()
    sess.headers.update({"User-Agent": USER_AGENT})
    return sess


def download_file(sess, url, dest, retries, read_timeout):
    """
    Download url -> dest with resume/skip and retries.

    Returns one of: "ok", "skip", "miss", "err". On "err" also returns a message.
    """
    last_err = ""
    for attempt in range(1, retries + 1):
        try:
            with sess.get(url, stream=True, timeout=(30, read_timeout)) as resp:
                if resp.status_code == 404:
                    return "miss", "404 not found"
                if resp.status_code != 200:
                    last_err = f"HTTP {resp.status_code}"
                    raise requests.RequestException(last_err)

                clen = resp.headers.get("Content-Length")
                remote_size = int(clen) if clen and clen.isdigit() else -1

                # Skip if the local file already matches the remote size.
                if remote_size > 0 and os.path.exists(dest):
                    if os.path.getsize(dest) == remote_size:
                        return "skip", ""

                tmp = dest + ".part"
                written = 0
                with open(tmp, "wb") as fh:
                    for chunk in resp.iter_content(chunk_size=1 << 16):
                        if chunk:
                            fh.write(chunk)
                            written += len(chunk)

                if remote_size > 0 and written != remote_size:
                    os.remove(tmp)
                    last_err = f"short read: {written}/{remote_size} bytes"
                    raise requests.RequestException(last_err)

                os.replace(tmp, dest)  # atomic rename
                return "ok", ""
        except requests.RequestException as exc:
            last_err = str(exc)
            if attempt < retries:
                time.sleep(attempt * 2)
    return "err", last_err


# --------------------------------------------------------------------------- #
# Orchestration
# --------------------------------------------------------------------------- #

_print_lock = Lock()


def run_downloads(base, out, ids, types, concurrency, retries, read_timeout):
    counts = {"ok": 0, "skip": 0, "miss": 0, "err": 0}
    counts_lock = Lock()

    def handle_dataset(dataset_id):
        sess = make_session(retries)
        results = []
        ddir = os.path.join(out, dataset_id)
        os.makedirs(ddir, exist_ok=True)
        for key in types:
            suffix, _ = FILE_TYPES[key]
            name = dataset_id + suffix
            url = f"{base.rstrip('/')}/static/data/{dataset_id}/{name}"
            dest = os.path.join(ddir, name)
            status, msg = download_file(sess, url, dest, retries, read_timeout)
            results.append((os.path.join(dataset_id, name), status, msg))
        return results

    with ThreadPoolExecutor(max_workers=concurrency) as pool:
        futures = {pool.submit(handle_dataset, d): d for d in ids}
        for fut in as_completed(futures):
            for relpath, status, msg in fut.result():
                with counts_lock:
                    counts[status] += 1
                with _print_lock:
                    if status == "ok":
                        print(f"[ok]   {relpath}")
                    elif status == "skip":
                        print(f"[skip] {relpath} (already complete)")
                    elif status == "miss":
                        print(f"[miss] {relpath} (404 - not published)")
                    else:
                        print(f"[err]  {relpath}: {msg}", file=sys.stderr)

    print(
        f"\nDone. downloaded={counts['ok']} skipped={counts['skip']} "
        f"missing={counts['miss']} errors={counts['err']}",
        file=sys.stderr,
    )
    return counts["err"] == 0


def print_catalog(ids, catalog):
    header = ["DATASET_ID", "CANCER", "SPECIES", "TREATMENT",
              "PATIENTS", "CELLS", "PLATFORM", "PRI/META", "PMID"]

    def row_for(dataset_id):
        info = catalog.get(dataset_id, {})
        return [
            dataset_id,
            cancer_from_id(dataset_id),
            info.get("species") or "-",
            info.get("treatment") or "-",
            info.get("patients") or "-",
            info.get("cells") or "-",
            info.get("platform") or "-",
            info.get("pri_meta") or "-",
            info.get("pmid") or "-",
        ]

    rows = [header] + [row_for(i) for i in ids]
    widths = [max(len(r[c]) for r in rows) for c in range(len(header))]
    for r in rows:
        print("  ".join(r[c].ljust(widths[c]) for c in range(len(header))).rstrip())

    print()
    for dataset_id in ids:
        cit = catalog.get(dataset_id, {}).get("citation")
        if cit:
            print(f"{dataset_id}: {cit}")


# --------------------------------------------------------------------------- #
# CLI
# --------------------------------------------------------------------------- #

def parse_types(value: str):
    keys = [t.strip().lower() for t in value.split(",") if t.strip()]
    if not keys:
        return list(FILE_TYPES.keys())
    for k in keys:
        if k not in FILE_TYPES:
            raise argparse.ArgumentTypeError(
                f"unknown type {k!r} (valid: {', '.join(FILE_TYPES)})"
            )
    return keys


def parse_datasets(value: str):
    return [d.strip() for d in value.split(",") if d.strip()]


def main():
    ap = argparse.ArgumentParser(
        description="Download single-cell datasets from the TISCH2 gallery.",
        formatter_class=argparse.ArgumentDefaultsHelpFormatter,
    )
    ap.add_argument("--out", default="tisch_data", help="Output directory")
    ap.add_argument("--datasets", type=parse_datasets, default=[],
                    help="Comma-separated dataset IDs (default: all in gallery)")
    ap.add_argument("--types", type=parse_types, default=list(FILE_TYPES.keys()),
                    help="Comma-separated artifact types: h5,exprzip,meta,de")
    ap.add_argument("--concurrency", type=int, default=4,
                    help="Datasets downloaded in parallel")
    ap.add_argument("--retries", type=int, default=3, help="Retry attempts per file")
    ap.add_argument("--timeout", type=int, default=300,
                    help="Per-file read timeout in seconds (between data chunks)")
    ap.add_argument("--base", default=DEFAULT_BASE, help="Base URL of the TISCH site")
    ap.add_argument("--list", action="store_true", help="List dataset IDs and exit")
    ap.add_argument("--info", action="store_true",
                    help="Print dataset metadata table and exit")
    ap.add_argument("--ipv4", action=argparse.BooleanOptionalAction, default=True,
                    help="Force IPv4 for downloads (TISCH IPv6 is often unroutable)")
    ap.add_argument("--headless", action=argparse.BooleanOptionalAction, default=True,
                    help="Run the Selenium browser headless")
    args = ap.parse_args()

    if args.ipv4:
        force_ipv4()

    concurrency = max(1, args.concurrency)
    retries = max(1, args.retries)

    # Decide whether we need to scrape the gallery with Selenium.
    need_scrape = args.list or args.info or not args.datasets

    ids = args.datasets
    catalog = {}
    if need_scrape:
        print("Scraping gallery with Selenium...", file=sys.stderr)
        ids, catalog = scrape_gallery(args.base, args.headless)
        # If the user asked for a subset while also needing the catalog (--info),
        # keep only the requested IDs.
        if args.datasets:
            ids = [d for d in args.datasets if d in catalog] or args.datasets
    ids = sorted(ids)
    print(f"Found {len(ids)} dataset(s).", file=sys.stderr)

    if args.list:
        for dataset_id in ids:
            print(dataset_id)
        return

    if args.info:
        print_catalog(ids, catalog)
        return

    os.makedirs(args.out, exist_ok=True)
    ok = run_downloads(args.base, args.out, ids, args.types,
                       concurrency, retries, args.timeout)
    sys.exit(0 if ok else 1)


if __name__ == "__main__":
    main()
