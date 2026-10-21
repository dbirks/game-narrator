#!/usr/bin/env bash
# scripts/gates/test-full-manifest.sh — the test-full evidence manifest
# (AGENTS.md evidence bundles, gap audit G12 / issue #33).
#
# test-full prints a human table; this writes the machine-readable companion:
# artifacts/gates/test-full-manifest.json, carrying one entry per gate
# (result + the same detail line the table shows) and the bench *second set's*
# report (ADR-0079, the second game's leg: the set id, the sample count, and
# each graded metric with its objective status) — the second game's holdout
# evidence lands in the test-full evidence manifest, not only in the bench
# gate's own artifacts.
#
# Usage: test-full-manifest.sh [MANIFEST_PATH [GATES_TSV]]
#
#   MANIFEST_PATH  default artifacts/gates/test-full-manifest.json
#   GATES_TSV      default artifacts/gates/test-full-gates.tsv — the
#                  tab-separated "gate <TAB> result <TAB> detail" rows the
#                  test-full recipe appends as it runs the gates. Missing or
#                  empty (a partial run) still yields a valid manifest.
#
# The bench second-set report (artifacts/bench-second/report.json) is embedded
# as a summary, not inlined: the manifest names the file and carries the set,
# split, run id, sample count and the per-metric (name, value, n, met) rows.
# When the report is absent — the bench gate NOT RUN (no tesseract) or not
# yet run — the entry records that instead of failing the manifest: the
# manifest is evidence of the run, and a NOT RUN is a fact about the host,
# not a manifest error. python3 (stdlib only) does the JSON; it is a dev-host
# gate dependency in the same class as the bench's own python3 use.

set -euo pipefail

manifest_path="${1:-artifacts/gates/test-full-manifest.json}"
gates_tsv="${2:-artifacts/gates/test-full-gates.tsv}"

command -v python3 >/dev/null 2>&1 || { echo "test-full-manifest: NOT RUN: python3 is required to write the evidence manifest" >&2; exit 2; }

mkdir -p "$(dirname "$manifest_path")"

# The git SHA and the toolchain stamp the manifest is written with: the same
# provenance fields the AGENTS.md evidence bundle lists, minus the ones the
# gate logs carry better (model ids, emulator build).
git_sha="$(git rev-parse --short HEAD 2>/dev/null || echo unknown)"
go_version="$(go version 2>/dev/null | awk '{print $3}' || echo unknown)"
go_os="$(go env GOOS 2>/dev/null || echo unknown)"
go_arch="$(go env GOARCH 2>/dev/null || echo unknown)"

MANIFEST_PATH="$manifest_path" GATES_TSV="$gates_tsv" GIT_SHA="$git_sha" \
GO_VERSION="$go_version" GO_OS="$go_os" GO_ARCH="$go_arch" \
BENCH_SECOND_DIR="artifacts/bench-second" \
python3 - <<'PY'
import json
import os
import sys
from datetime import datetime, timezone

manifest_path = os.environ["MANIFEST_PATH"]
gates_tsv = os.environ["GATES_TSV"]
bench_second_dir = os.environ["BENCH_SECOND_DIR"]

# The per-gate rows, exactly as the test-full recipe judged them. A missing
# file is a partial run: the manifest still writes, with an empty gate list
# and the reason recorded.
gates = []
if os.path.isfile(gates_tsv):
    with open(gates_tsv, encoding="utf-8") as fh:
        for line in fh:
            parts = line.rstrip("\n").split("\t")
            if len(parts) < 3:
                continue
            gates.append({"gate": parts[0], "result": parts[1], "detail": parts[2]})
gates_note = None if os.path.isfile(gates_tsv) else "no gate rows recorded (partial run?)"

# The bench second set's report: the second game's holdout leg (ADR-0079).
# Summary, not inline: name the files, carry the metrics.
second_report = os.path.join(bench_second_dir, "report.json")
second_manifest = os.path.join(bench_second_dir, "manifest.json")
if os.path.isfile(second_report):
    with open(second_report, encoding="utf-8") as fh:
        report = json.load(fh)
    bench_second = {
        "present": True,
        "report": "artifacts/bench-second/report.json",
        "report_md": "artifacts/bench-second/report.md" if os.path.isfile(os.path.join(bench_second_dir, "report.md")) else None,
        "run_manifest": "artifacts/bench-second/manifest.json" if os.path.isfile(second_manifest) else None,
        "set": report.get("set"),
        "split": report.get("split"),
        "origin": report.get("origin"),
        "class": report.get("class"),
        "run_id": report.get("run_id"),
        "samples": report.get("samples"),
        "generated_at": report.get("generated_at"),
        "metrics": [
            {"name": m.get("name"), "value": m.get("value"), "n": m.get("n"), "met": m.get("met")}
            for m in report.get("metrics", [])
        ],
    }
else:
    bench_second = {
        "present": False,
        "reason": "artifacts/bench-second/report.json absent: the bench gate was NOT RUN on this host (or test-full ran it in an order that leaves it unrun)",
    }

manifest = {
    "schema_version": 1,
    "name": "test-full evidence manifest",
    "generated_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
    "git_sha": os.environ.get("GIT_SHA", "unknown"),
    "go_version": os.environ.get("GO_VERSION", "unknown"),
    "os": os.environ.get("GO_OS", "unknown"),
    "arch": os.environ.get("GO_ARCH", "unknown"),
    "gates": gates,
    "gates_note": gates_note,
    "bench_second": bench_second,
}

with open(manifest_path, "w", encoding="utf-8") as fh:
    json.dump(manifest, fh, indent=2)
    fh.write("\n")

print(f"test-full evidence manifest written: {manifest_path} (gates={len(gates)}, bench_second={'present' if bench_second['present'] else 'absent'})")
PY
