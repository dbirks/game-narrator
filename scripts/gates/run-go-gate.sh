#!/bin/sh
# run-go-gate.sh NAME GO-TEST-ARGS...
#
# Runs `go test -json GO-TEST-ARGS...` and summarises the result per test:
# PASS / FAIL / SKIP / NOT RUN. A SKIP whose message starts with "NOT RUN"
# (the gate convention for "capability not configured on this host") is
# reported as NOT RUN and its message is printed as the setup instruction;
# see docs/dev-environment.md, "Gates".
#
# Artifacts (artifacts/ is gitignored):
#   artifacts/gates/NAME.xml     JUnit-style summary
#   artifacts/gates/NAME.stderr  raw go test stderr (build errors etc.)
#   artifacts/gates/NAME.json    raw go test -json stream
#
# Exit codes:
#   0  every test ran and passed
#   1  any test failed (or go test itself failed / nothing ran)
#   2  nothing failed but at least one test was NOT RUN
#
# Run from the module root (the go-junit-report binary is used for the XML
# summary if installed; otherwise the scripts/gates/junit summariser is).
set -u

if [ $# -lt 2 ]; then
	echo "usage: run-go-gate.sh NAME GO-TEST-ARGS..." >&2
	exit 1
fi
name=$1
shift

outdir=artifacts/gates
mkdir -p "$outdir"
json="$outdir/$name.json"
xml="$outdir/$name.xml"
stderr="$outdir/$name.stderr"

go test -json "$@" >"$json" 2>"$stderr"
goc=$?

if command -v go-junit-report >/dev/null 2>&1; then
	go-junit-report <"$json" >"$xml" 2>>"$stderr"
	# Still print the per-test PASS/FAIL/SKIP/NOT RUN summary so the gate
	# output stays readable and NOT RUN setup instructions are visible.
	go run ./scripts/gates/junit /dev/null <"$json"
else
	go run ./scripts/gates/junit "$xml" <"$json"
fi
rc=$?

# go test failing to build (or exiting non-zero otherwise) is a gate
# failure even when the summariser classified its output as NOT RUN-only.
if [ "$goc" -ne 0 ] && [ "$rc" -eq 2 ]; then
	rc=1
fi
if [ "$goc" -ne 0 ] && [ "$rc" -eq 0 ]; then
	rc=1
fi
exit "$rc"
