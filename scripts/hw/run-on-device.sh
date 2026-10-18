#!/bin/sh
# run-on-device.sh USER@HOST
#
# Runs the hardware smoke gate (internal/hwsmoke) on a remote device — the
# UNO Q appliance — from the dev host:
#
#   1. cross-compiles the hwsmoke test binary (GOOS=linux GOARCH=arm64,
#      CGO_ENABLED=0 — the release build mode; the binary runs standalone on
#      the device and needs no module checkout there),
#   2. copies it to the device's /tmp (the only place this script may write),
#   3. runs it there with -test.v and GAME_NARRATOR_HW_SMOKE=1 (the gate the
#      tests require to run at all — see internal/hwsmoke.gate), with the
#      service environment loaded the same way the installed service gets it
#      (sudo + /etc/game-narrator/env, never printed) and the data dir
#      pinned to /tmp (the device is read-only outside /tmp: the 60 s
#      serve session and the data-dir probe must not touch service state),
#   4. pulls the output back and summarises it with the same junit
#      summariser the local gate uses, so artifacts/gates/
#      test-hardware-device.{xml,rc} follow the documented 0/1/2 exit
#      contract (scripts/gates/run-go-gate.sh): 0 every check ran and
#      passed, 1 a check failed, 2 nothing failed but at least one check
#      was NOT RUN (the setup instruction is printed by the summariser).
#
# The remote run only ever: reads the installed binary and service env,
# writes its own test binary and output under /tmp, and runs the cross-
# compiled test. It never touches sshd, sshd_config, authorized_keys or the
# device's network configuration, and never reinstalls the service.
#
# Usage (from the repo root or anywhere):
#
#   scripts/hw/run-on-device.sh arduino@10.0.0.139
#
# Environment:
#   GN_HW_SMOKE_TIMEOUT  -test.timeout passed to the remote run (default 15m;
#   the 60 s serve session needs headroom on a 4-core appliance)

set -u

if [ $# -ne 1 ]; then
	echo "usage: $0 USER@HOST  (e.g. $0 arduino@10.0.0.139)" >&2
	exit 1
fi
host=$1

root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd) || exit 1
cd "$root" || exit 1

remote_bin=/tmp/hwsmoke-test
remote_out=/tmp/hwsmoke-out.txt
remote_data=/tmp/hwsmoke-data
timeout=${GN_HW_SMOKE_TIMEOUT:-15m}

tmp=$(mktemp -d) || exit 1
trap 'rm -rf "$tmp"' EXIT

fail() {
	echo "run-on-device: $*" >&2
	exit 1
}

command -v go >/dev/null 2>&1 || fail "go is not on PATH"
command -v ssh >/dev/null 2>&1 || fail "ssh is not on PATH"
command -v scp >/dev/null 2>&1 || fail "scp is not on PATH"

echo "run-on-device: cross-compiling the hwsmoke test binary (linux/arm64)"
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o "$tmp/hwsmoke.test" ./internal/hwsmoke \
	|| fail "go test -c failed: the gate binary does not build"

echo "run-on-device: copying to $host:$remote_bin"
scp "$tmp/hwsmoke.test" "$host:$remote_bin" || fail "scp to the device failed"
ssh "$host" "chmod 755 $remote_bin" || fail "chmod on the device failed"

echo "run-on-device: running the gate on $host (timeout $timeout)"
# The remote command: a fresh /tmp data dir (device read-only policy), the
# service env loaded under sudo when the file exists (the appliance install;
# its token never leaves the process environment), and the gate env var.
# The data-dir pin goes on the command line so it wins over whatever the
# service env sets.
ssh "$host" "set -u; rm -rf $remote_data $remote_out; mkdir -p $remote_data; if [ -f /etc/game-narrator/env ]; then sudo -n sh -c 'set -a; . /etc/game-narrator/env; set +a; GAME_NARRATOR_HW_SMOKE=1 GAME_NARRATOR_DATA_DIR=$remote_data $remote_bin -test.v -test.timeout $timeout'; else GAME_NARRATOR_HW_SMOKE=1 GAME_NARRATOR_DATA_DIR=$remote_data $remote_bin -test.v -test.timeout $timeout; fi" \
	2>&1 | tee "$tmp/verbose.txt"
# POSIX sh loses the ssh exit code through the pipe; the summariser is the
# source of truth for 0/1/2, and a dead connection shows up as an empty or
# truncated stream, which the summariser classifies as a failure (1).

echo "run-on-device: summarising"
outdir=artifacts/gates
mkdir -p "$outdir"
json="$tmp/gotest.json"
awk -v pkg=internal/hwsmoke -f scripts/hw/verbose-to-gotest-json.awk "$tmp/verbose.txt" >"$json"

go build -o "$tmp/gn-junit" ./scripts/gates/junit || fail "failed to build scripts/gates/junit"
"$tmp/gn-junit" "$outdir/test-hardware-device.xml" <"$json"
rc=$?

# A run that produced no test events at all (ssh died, the binary could not
# start, -test.v printed nothing) is a gate failure, not a 0/2.
if [ ! -s "$json" ]; then
	echo "run-on-device: no test output came back from $host — the remote run did not complete" >&2
	rc=1
fi
echo "$rc" >"$outdir/test-hardware-device.rc"

echo "run-on-device: artifacts/gates/test-hardware-device.{xml,rc} written (exit $rc)"
exit "$rc"
