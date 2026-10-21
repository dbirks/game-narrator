#!/usr/bin/env bash
# adr-numbers.sh — ADR numbering gate (AGENTS.md rule 2: numbered, immutable
# ADRs in docs/adr/NNNN-brief-name.md, one decision per number).
#
# docs/adr grows from parallel branches; this gate catches the failure where
# two branches claim the same number (the 2026-10-20 four-way 0109 collision).
#
# Rules (checked over every entry of docs/adr):
#   dup-number   two files carry the same leading 4-digit NNNN
#   bad-name     an entry that is neither README.md nor 0000-TEMPLATE.md and
#                does not match ^[0-9]{4}-[a-z0-9]+(-[a-z0-9]+)*\.md$
#                (lowercase slug; digits inside a slug are fine, e.g. 0032-m5-...)
#   not-file     a subdirectory where an ADR file is expected
#
# Findings print "rule<TAB>entry".
#
# Exit codes: 0 clean, 1 findings, 2 NOT RUN (docs/adr missing) / usage error.
#
# Modes: --self-test (plant known-good and known-bad samples in a temp dir and
# verify the checker classifies every one; exits 0 only if every planted
# violation is found, the clean tree stays clean, and the exit codes are as
# expected).

set -u

DEFAULT_ADR_DIR=docs/adr
SELFTEST=0

usage() {
	echo "usage: adr-numbers.sh [--self-test]" >&2
	exit 2
}

# check_dir DIR — print one finding per violation and return 0 clean /
# 1 findings. DIR must exist (NOT RUN is the caller's concern).
check_dir() {
	local dir=$1
	local entries path name num rc=0
	declare -A seen=()
	entries=$(find "$dir" -mindepth 1 -maxdepth 1 | LC_ALL=C sort)
	while IFS= read -r path; do
		[ -n "$path" ] || continue
		name=${path##*/}
		if [ ! -f "$path" ]; then
			echo "not-file	$name"
			rc=1
			continue
		fi
		case $name in
		README.md | 0000-TEMPLATE.md) continue ;;
		esac
		if ! printf '%s\n' "$name" | grep -Eq '^[0-9]{4}-[a-z0-9]+(-[a-z0-9]+)*\.md$'; then
			echo "bad-name	$name"
			rc=1
			continue
		fi
		num=${name%%-*}
		if [ -n "${seen[$num]:-}" ]; then
			echo "dup-number	$name (conflicts with ${seen[$num]})"
			rc=1
		else
			seen[$num]=$name
		fi
	done <<EOF
$entries
EOF
	return $rc
}

run_self_test() {
	local tmp out rc label needle fail=0
	tmp=$(mktemp -d) || { echo "adr-numbers: self-test: mktemp failed" >&2; return 2; }
	local clean=$tmp/clean bad=$tmp/bad
	mkdir -p "$clean" "$bad"

	# --- clean tree: well-formed unique numbers + the two allowed entries ---
	printf '# ADR-0001: A\n' > "$clean/0001-first-decision.md"
	printf '# ADR-0002: B\n' > "$clean/0002-second-decision.md"
	printf '# ADR-0032: C\n' > "$clean/0032-m5-part1-digits-in-slug.md"
	printf '# template\n' > "$clean/0000-TEMPLATE.md"
	printf '# index\n' > "$clean/README.md"

	# --- bad tree: one sample of each rule, plus a duplicate pair ----------
	printf 'x\n' > "$bad/0001-good-slug.md"
	printf 'x\n' > "$bad/0002-first-of-dup-pair.md"
	printf 'x\n' > "$bad/0002-second-of-dup-pair.md"
	printf 'x\n' > "$bad/0003-UppercaseSlug.md"
	printf 'x\n' > "$bad/10004-five-digits.md"
	printf 'x\n' > "$bad/0005-slug-with_space.md"
	printf 'x\n' > "$bad/notes.txt"
	mkdir -p "$bad/9999-a-directory"

	echo "adr-numbers: self-test: clean tree"
	if out=$(check_dir "$clean" 2>&1); then
		if [ -n "$out" ]; then
			echo "self-test FAIL: clean tree produced findings: $out" >&2
			fail=1
		fi
	else
		echo "self-test FAIL: clean tree expected rc=0, got $?" >&2
		fail=1
	fi

	echo "adr-numbers: self-test: bad tree"
	out=$(check_dir "$bad" 2>&1)
	rc=$?
	if [ "$rc" -ne 1 ]; then
		echo "self-test FAIL: bad tree expected rc=1, got $rc" >&2
		fail=1
	fi
	expect_finding() {
		label=$1
		needle=$2
		case $out in
		*"$needle"*) : ;;
		*) echo "self-test FAIL: $label — expected finding missing: $needle" >&2; fail=1 ;;
		esac
	}
	expect_finding "dup-number" 'dup-number	0002-second-of-dup-pair.md (conflicts with 0002-first-of-dup-pair.md)'
	expect_finding "bad-name uppercase" 'bad-name	0003-UppercaseSlug.md'
	expect_finding "bad-name five digits" 'bad-name	10004-five-digits.md'
	expect_finding "bad-name space in slug" 'bad-name	0005-slug-with_space.md'
	expect_finding "bad-name non-md entry" 'bad-name	notes.txt'
	expect_finding "not-file directory" 'not-file	9999-a-directory'
	# the clean entry in the bad tree must not be flagged itself
	case $out in
	*0001-good-slug*) echo "self-test FAIL: 0001-good-slug.md was flagged" >&2; fail=1 ;;
	esac

	rm -rf "$tmp"
	if [ $fail -eq 0 ]; then
		echo "adr-numbers: self-test PASS (clean tree clean; every planted violation classified)"
		return 0
	fi
	echo "adr-numbers: self-test FAILED" >&2
	return 1
}

case "${1:-}" in
--self-test) SELFTEST=1 ;;
-h | --help) usage ;;
'') : ;;
*) usage ;;
esac

if [ $SELFTEST -eq 1 ]; then
	run_self_test
	exit $?
fi

adr_dir=$DEFAULT_ADR_DIR
if [ ! -d "$adr_dir" ]; then
	echo "adr-numbers: NOT RUN — $adr_dir does not exist" >&2
	exit 2
fi

if out=$(check_dir "$adr_dir"); then
	count=$(find "$adr_dir" -mindepth 1 -maxdepth 1 -type f -name '*.md' | wc -l | tr -d ' ')
	echo "adr-numbers: OK ($count entries in $adr_dir, numbers unique, names well-formed)"
	exit 0
fi

echo "$out"
echo "adr-numbers: FAILED — renumber the colliding file to the next free NNNN and keep each filename NNNN-slug.md (see docs/adr/README.md)" >&2
exit 1
