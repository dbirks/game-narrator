#!/usr/bin/env bash
# secret-scan.sh — "no secrets / private endpoints / game data in Git" gate
# (AGENTS.md rule 5; docs/status/acceptance.md M0).
#
# Scans tracked files (`git ls-files`) and, with --history, the added lines of
# every commit (`git log -p --all`) for:
#   token-prefix   vendor token shapes: sk-, ghp_/gho_/ghr_, github_pat_,
#                  xoxb-/xoxp-, Google AIza..., AWS AKIA..., JWT eyJ...
#   high-entropy   key/token/secret/password/credential/auth assigned to a
#                  value of >= 20 chars with >= 3 character classes
#   pem            PEM private key blocks
#   private-url    http(s) URL with an RFC 1918 host (10/8, 172.16/12,
#                  192.168/16) or a .local/.lan/.internal/.home/.ts.net host
#   private-host   a bare RFC 1918 IPv4 address anywhere a URL scheme is not
#                  required — user@ip, ip:port, plain text — with octets
#                  validated to 0-255 (RFC 5737 doc ranges are not RFC 1918
#                  and cannot match; loopback/CGNAT/public addresses cannot
#                  match). Catches the leak private-url misses by design
#                  (commit 398cad0: a bare user@10.x.x.x ssh target).
#   home-path      absolute /home/<user>/ path
#   game-data      tracked file with a game-image extension or magic bytes
#                  (CISO/RARC/Yaz0 at offset 0, ISO 9660 "CD001" at 32769)
#   big-binary     tracked binary file >= the size threshold (default 1 MiB)
#                  not covered by the allowlist
#
# Documented false positives are suppressed with scripts/gates/secret-scan.allow
# (tab-separated: path-glob <TAB> value-ERE-or-* <TAB> reason). In --history
# mode the path may be commit-scoped — commit=<sha-prefix>:<path-glob> — so a
# finding in an already-pushed commit that has since been fixed can be blessed
# for that commit alone; the gate still fails on any NEW occurrence (a new
# commit is a new sha and matches no commit= entry).
#
# Findings print path:line and the rule — NEVER the matched value (it is
# masked; only its length is reported).
#
# Exit codes: 0 clean, 1 findings, 2 NOT RUN (no git, no worktree, or an
# explicitly given --allow file is missing).
#
# Modes: --history (also scan the added lines of every commit), --self-test
# (plant known-good and known-bad samples in a temp git repo and verify the
# scanner classifies every one; exits 0 only if every classification is
# correct, the scanner's exit code is 1, and no value is printed).

set -u

DEFAULT_ALLOW=scripts/gates/secret-scan.allow
BIG_BINARY_MAX_KB=1024
HISTORY=0
SELFTEST=0
ALLOW_OVERRIDE=""
ALLOW_FILE=""
FINDINGS=0
ALLOWED_NOTES=""
IN_GHA=${GITHUB_ACTIONS:-false}

# --- text rules (name<TAB>ERE) -------------------------------------------------
# The patterns are metacharacter shapes (no sample secrets), so this file does
# not trip its own rules; the allowlist still documents it explicitly.
RULES_TEXT='token-prefix	\bsk-[A-Za-z0-9_-]{16,}|ghp_[A-Za-z0-9]{20,}|gho_[A-Za-z0-9]{20,}|ghr_[A-Za-z0-9]{20,}|github_pat_[A-Za-z0-9_]{20,}|xox[bp]-[A-Za-z0-9-]{10,}|AIza[0-9A-Za-z_-]{20,}|AKIA[0-9A-Z]{16}|eyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{5,}
high-entropy	(api[_-]?key|apikey|secret|token|passw?o?r?d?|credential|auth[a-z_]*)(["'"'"' ]?[[:space:]]*[:=][[:space:]]*["'"'"' ]?[A-Za-z0-9+/_=.-]{20,})
pem	-----BEGIN[ A-Z]*PRIVATE KEY-----
private-url	https?://10\.[0-9]+\.[0-9]+\.[0-9]+|https?://192\.168\.[0-9]+\.[0-9]+|https?://172\.(1[6-9]|2[0-9]|3[0-1])\.[0-9]+\.[0-9]+|https?://[^/ :"]*\.(local|lan|internal|home|ts\.net)([:/""'"'"' ]|[^a-z0-9])|https?://[^/ :"]*\.(local|lan|internal|home|ts\.net)$
private-host	(^|[^0-9A-Za-z.])(10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}|192\.168\.[0-9]{1,3}\.[0-9]{1,3})([^0-9A-Za-z.]|$)
home-path	/home/[a-z0-9._-]{2,}/'

GAME_EXT_RE='\.(iso|gcm|rvz|ciso|wbfs|7z|nkit\.iso|dtm|wia|sav|szs|gcz|drc)$'
MAGIC_CDO01=4344303031 # "CD001" at offset 32769 (ISO 9660 volume descriptor)

usage() {
	echo "usage: secret-scan.sh [--history] [--self-test] [--allow FILE]" >&2
}

# --- allowlist ------------------------------------------------------------------
# Entry format (tab-separated): path-glob<TAB>value-ERE-or-`*`<TAB>reason.
# In --history mode path-glob may be commit=<sha-prefix>:<path-glob>, which
# then matches only that commit (sha prefix match); allowlist_match RULE
# [REF] PATH VALUE prints the reason when suppressed, else "". REF is the
# short sha for history findings and empty for tree/file findings.
allowlist_match() {
	[ -n "$ALLOW_FILE" ] && [ -f "$ALLOW_FILE" ] || return 0
	local rule=$1 ref=${2:-} path=$3 value=$4
	awk -F'\t' -v rule="$rule" -v ref="$ref" -v path="$path" -v value="$value" '
		NF >= 3 && $1 !~ /^#/ && $1 == rule {
			pat = $2
			commitok = 1
			if (ref != "" && pat ~ /^commit=/) {
				sub(/^commit=/, "", pat)
				csep = index(pat, ":")
				if (csep == 0) commitok = 0
				else {
					csha = substr(pat, 1, csep - 1)
					pat = substr(pat, csep + 1)
					if (substr(ref, 1, length(csha)) != csha) commitok = 0
				}
			}
			if (!commitok) next
			if (path !~ pat) next
			if ($3 == "*" || $3 == "") ok = 1
			else ok = (value ~ $3)
			if (ok) {
				reason = $0
				sub(/^[^\t]*\t[^\t]*\t[^\t]*\t/, "", reason)
				print reason
				exit
			}
		}
	' "$ALLOW_FILE" 2>/dev/null
}

# --- GitHub Actions annotations (mirror scripts/gates/annotate-go.sh) -----------
# Under GITHUB_ACTIONS, allowlisted items are emitted as ::notice and real
# findings as ::error with file=/title=; the matched value is NEVER printed
# (masking is done by the caller, as in the plain-line report below).
# Escaping mirrors annotate-go.sh: properties escape % : , ; data escapes % .
# When FILE is empty the helper emits a plain ::notice::<message> (summary
# lines). All args default so callers are set -u safe, and both return 0.
esc_data() { printf '%s' "${1:-}" | sed -e 's/%/%25/g'; }
esc_prop() { printf '%s' "${1:-}" | sed -e 's/%/%25/g' -e 's/:/%3A/g' -e 's/,/%2C/g'; }
gha_error() { # $1=file $2=title $3=message
	case "$IN_GHA" in
		true|True|TRUE) printf '::error file=%s,title=%s::%s\n' \
			"$(esc_prop "${1:-}")" "$(esc_prop "${2:-}")" "$(esc_data "${3:-}")" ;;
	esac
	return 0
}
gha_notice() { # $1=file $2=title $3=message (file may be empty)
	case "$IN_GHA" in
		true|True|TRUE)
			if [ -n "${1:-}" ]; then
				printf '::notice file=%s,title=%s::%s\n' \
					"$(esc_prop "$1")" "$(esc_prop "${2:-}")" "$(esc_data "${3:-}")"
			else
				printf '::notice::%s\n' "$(esc_data "${3:-}")"
			fi
			;;
	esac
	return 0
}

# --- finding report ---------------------------------------------------------------
# report MODE KIND REF PATH LINE RULE VALUE
#   MODE: tree | hist (hist prefixes the commit sha to the location)
#   KIND: line (VALUE is a matched secret) | file (VALUE is a detail string)
report() {
	local mode=$1 kind=$2 ref=$3 path=$4 line=$5 rule=$6 value=$7
	local detail
	detail=$(allowlist_match "$rule" "$ref" "$path" "$value")
	if [ -n "$detail" ]; then
		# Plain-note format constraint (CI, setup-go problem matcher): the
		# `go` matcher registered by actions/setup-go is
		# ^\s*(.+\.go):(?:(\d+):(\d+):)? (.*), so any plain line of the
		# shape `path.go: message` becomes a FAILURE-level check annotation
		# with no title. The path is therefore never followed by `: ` —
		# the rule sits in parens instead. Keep it that way (and keep the
		# allow file's reason fields free of a `file.go:` sequence).
		ALLOWED_NOTES="${ALLOWED_NOTES}  - $path ($rule) — allowlisted: $detail
"
		# allowlisted: ::notice under GHA (never ::error)
		gha_notice "$path" "secret-scan allowlisted:$rule" "allowlisted: $detail"
		return 0
	fi
	local where shown
	if [ "$mode" = tree ]; then
		where="$path:$line"
	else
		where="$ref $path:$line"
	fi
	if [ "$kind" = file ]; then
		shown="$value"
	else
		shown="[masked, len=${#value}]"
	fi
	FINDINGS=$((FINDINGS + 1))
	echo "secret-scan: $where: $rule $shown"
	# real finding: ::error under GHA; file= the repo-relative path so the
	# annotation attaches; the message never contains the matched value
	gha_error "$path" "secret-scan::$rule $where" "$shown"
}

# entropy_ok VALUE: success when VALUE has >= 20 chars, >= 3 character
# classes (lowercase / uppercase / digit / other) and at least one digit.
# The digit requirement keeps identifier-like values (e.g. a TS property name
# assigned across the line) from tripping the rule.
entropy_ok() {
	printf '%s' "$1" | awk '{
		v = $0
		lc = (v ~ /[a-z]/) ? 1 : 0
		uc = (v ~ /[A-Z]/) ? 1 : 0
		dg = (v ~ /[0-9]/) ? 1 : 0
		ot = v
		gsub(/[a-zA-Z0-9]/, "", ot)
		ot = (ot != "") ? 1 : 0
		exit !(length(v) >= 20 && lc + uc + dg + ot >= 3 && dg)
	}'
}

# rule_matches RULE PATTERN LINE: success when the line has a match that
# passes RULE's gate (the entropy check for high-entropy). Sets MATCH_VALUE.
rule_matches() {
	local rule=$1 pattern=$2 line=$3
	local m
	MATCH_VALUE=""
	m=$(printf '%s\n' "$line" | grep -oIE -e "$pattern" | head -n 1) || return 1
	[ -n "$m" ] || return 1
	if [ "$rule" = high-entropy ]; then
		local v
		# strip keyword prefix + separator/quote so only the assigned value
		# is judged
		v=$(printf '%s\n' "$m" | sed -E "s/^[^:]*[:=][[:space:]]*['\"]?//")
		[ -n "$v" ] || v=$m
		entropy_ok "$v" || return 1
		MATCH_VALUE=$v
	elif [ "$rule" = private-host ]; then
		# The pattern's boundary group makes grep -o's match over- or
		# under-report the bare address, so re-derive it, reject a match
		# embedded in a longer IP/hostname (110.1.2.3, 10.1.2.3.4), and
		# validate the octets (the ERE admits 0-999 per octet; 198.51.100.x
		# / 192.0.2.x / 203.0.113.x cannot match the prefix at all — they
		# are RFC 5737, not RFC 1918).
		local ip o pre post left right
		ip=$(printf '%s\n' "$m" | grep -oE '10\.[0-9]{1,3}\.[0-9]{1,3}\.[0-9]{1,3}|172\.(1[6-9]|2[0-9]|3[01])\.[0-9]{1,3}\.[0-9]{1,3}|192\.168\.[0-9]{1,3}\.[0-9]{1,3}' | head -n 1)
		[ -n "$ip" ] || return 1
		# judge the neighbours of the bare address itself (grep -o's match
		# may have consumed a boundary char, so the match's own ends are not
		# the address's ends)
		pre=${line%%"$ip"*}
		post=${line#*"$ip"}
		left=${pre: -1}
		right=${post:0:1}
		case $left in [0-9A-Za-z.]) return 1 ;; esac
		case $right in [0-9A-Za-z.]) return 1 ;; esac
		for o in ${ip//./ }; do
			case $o in '' | *[!0-9]) return 1 ;; esac
			[ "$o" -le 255 ] || return 1
		done
		MATCH_VALUE=$ip
	else
		MATCH_VALUE=$m
	fi
	return 0
}

# --- tree scan ---------------------------------------------------------------------
scan_tree() {
	local rule pattern out line path rest
	while IFS=$'\t' read -r rule pattern; do
		[ -n "$rule" ] || continue
		# the wrapper keeps xargs running past no-match chunks (a grep exit 1
		# would otherwise abort the remaining file chunks) and hides grep's
		# exit code so `|| continue` only fires on real pipeline failures
		# shellcheck disable=SC2016 # $1/$@ expand inside the sh -c context
		out=$(git ls-files -z | xargs -0 -r sh -c 'grep -nIE -e "$1" -- "$@" || true' sh "$pattern" 2>/dev/null) || continue
		[ -n "$out" ] || continue
		while IFS= read -r line; do
			path=${line%%:*}
			rest=${line#*:}
			case $rest in
				[0-9]*:*) ;;
				*) continue ;;
			esac
			if rule_matches "$rule" "$pattern" "${rest#*:}"; then
				report tree line "" "$path" "${rest%%:*}" "$rule" "$MATCH_VALUE"
			fi
		done < <(printf '%s\n' "$out")
	done <<< "$RULES_TEXT"
}

# --- history scan --------------------------------------------------------------------
# hist_lines: `sha<TAB>path<TAB>lineno<TAB>content` per ADDED line of every
# commit (lineno is the new-file line number from the @@ hunk header).
# Context lines (space-prefixed) stay inside the hunk and advance the
# new-file line number; only non-diff lines (commit header, message, blank
# lines, diff --git/index) end the section. This matters: an added line
# preceded by a context line is exactly where a real leak hid (398cad0
# docs/manuals: the bare ssh target sat right after a context line).
hist_lines() {
	git log -p --all 2>/dev/null | awk '
		/^commit /      { sha = substr($2, 1, 12); file = ""; inplus = 0; next }
		/^--- /         { inplus = 0; next }
		/^\+\+\+ \/dev\/null/ { file = ""; inplus = 0; next }
		/^\+\+\+ /      {
			inplus = 1
			p = substr($0, 5)
			if (p ~ /^b\//) file = substr(p, 3); else file = p
			nl = 0
			next
		}
		/^@@ /        {
			if (inplus && file != "") {
				s = $0
				if (match(s, /\+[0-9]+(,[0-9]+)?/)) {
					t = substr(s, RSTART + 1, RLENGTH - 1)
					sub(/,.*$/, "", t)
					nl = t + 0
				}
			}
			next
		}
		/^ /          { if (inplus) nl++; next }
		/^[^+-]/      { file = ""; inplus = 0; next }
		/^\+[^+]/     {
			if (inplus && file != "" && file != "/dev/null") {
				print sha "\t" file "\t" nl "\t" substr($0, 2)
				nl++
			}
			next
		}
		/^-/          { next }
	'
}

scan_history() {
	local hfile rule pattern cline sha path rest line content
	# one `git log -p --all` pass into a temp file; per rule, only the content
	# column (field 4) is regex-filtered, so only candidate lines pay for value
	# extraction. The pattern is passed via the environment because awk -v
	# would mangle its backslashes.
	hfile=$(mktemp) || {
		echo "NOT RUN: secret-scan (mktemp failed)" >&2
		exit 2
	}
	hist_lines >"$hfile"
	while IFS=$'\t' read -r rule pattern; do
		[ -n "$rule" ] || continue
		while IFS=$'\t' read -r cline; do
			sha=${cline%%$'\t'*}
			rest=${cline#*$'\t'}
			path=${rest%%$'\t'*}
			rest=${rest#*$'\t'}
			line=${rest%%$'\t'*}
			content=${rest#*$'\t'}
			if rule_matches "$rule" "$pattern" "$content"; then
				report hist line "$sha" "$path" "$line" "$rule" "$MATCH_VALUE"
			fi
		done < <(GN_RULE_PATTERN="$pattern" awk -F'\t' '
			BEGIN { pat = ENVIRON["GN_RULE_PATTERN"] }
			NF >= 4 {
				# the content is fields 4..NF joined: added lines that start
				# with tabs (indented Go) would otherwise sit in $5+ and the
				# $4 filter would silently drop them
				c = $4
				for (i = 5; i <= NF; i++) c = c "\t" $i
				if (c ~ pat) print
				}
			' "$hfile")
	done <<< "$RULES_TEXT"
	rm -f "$hfile"
}

# --- game-data / big-binary file scan -----------------------------------------------
# is_text_file FILE: success for text files (grep -I treats binaries as no match).
is_text_file() {
	grep -qI . -- "$1" 2>/dev/null
}

scan_files() {
	local path size head mid name want_hex
	while IFS= read -r path; do
		[ -f "$path" ] || continue
		if [[ ${path,,} =~ $GAME_EXT_RE ]]; then
			report file tree "" "$path" 0 game-data "game-image extension"
			continue
		fi
		size=$(wc -c < "$path" 2>/dev/null) || continue
		# magic bytes are only worth checking files big enough to contain them
		if [ "${size:-0}" -ge 5 ]; then
			head=$(od -An -tx1 -N 4 -- "$path" 2>/dev/null | tr -d ' \n')
			mid=$(od -An -tx1 -j 32768 -N 6 -- "$path" 2>/dev/null | tr -d ' \n')
			if [ -n "$head" ]; then
				for name in CISO RARC Yaz0; do
					want_hex=$(printf '%s' "$name" | od -An -tx1 | tr -d ' \n')
					if [ "${head:0:4}" = "$want_hex" ]; then
						report file tree "" "$path" 0 game-data "magic $name at offset 0"
						break 2
					fi
				done
			fi
			if [ "${mid:0:4}" = "$MAGIC_CDO01" ] || [ "${mid:1:4}" = "$MAGIC_CDO01" ]; then
				report file tree "" "$path" 0 game-data "magic CD001 (ISO 9660) at offset 32769"
			fi
		fi
		if [ "${size:-0}" -ge 5 ] && ! is_text_file "$path"; then
			size_kb=$(( size / 1024 ))
			if [ "$size_kb" -ge "$BIG_BINARY_MAX_KB" ]; then
				report file tree "" "$path" 0 big-binary "${size_kb} KiB binary"
			fi
		fi
	done < <(git ls-files -z | tr '\0' '\n')
}

# --- self-test -------------------------------------------------------------------------
# Plants synthetic samples in a throwaway git repo and verifies every one is
# detected (with a masked value), the clean file is untouched, the exit code
# is 1, and no secret value is ever printed.
self_test() {
	local tmp trepo rc out fail selfscan
	tmp=$(mktemp -d) || { echo "secret-scan: self-test: mktemp failed" >&2; return 2; }
	trap 'rm -rf "$tmp"' EXIT
	trepo="$tmp/repo"
	mkdir -p "$trepo"
	(
		cd "$trepo" || exit 2
		git init -q . || exit 2
		git config user.email selftest@example.invalid
		git config user.name "secret-scan self-test"
		# every sample below is synthetic and MUST be detected
		cat > secrets.txt <<'SECRETS'
# planted samples for the self-test — all synthetic, all must be detected
openai_key=sk-plantedselftest0123456789ab
gh=ghp_plantedselftest0123456789
aws=AKIAQPLANTEDSELF0000
slack=xoxb-planted-self-123456
google=AIzaSyAplantedSelfTest0123456
jwt=eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJwbGFudGVkIn0.planted_signature_value
pemkey=-----BEGIN RSA PRIVATE KEY-----
private_url=https://192.168.42.7:8443/v1
local_host=https://gateway.lan:8443/v1
tailscale=https://node123.ts.net/api
home=/home/planteduser/project/x
ssh_target=ssh arduino@10.1.2.3
bare_with_port=192.168.4.5:22
SECRETS
		git add secrets.txt
		git commit -qm "plant secrets"
		# synthetic CISO magic (game data)
		printf 'CISO%s' "$(head -c 4096 /dev/zero | tr '\0' 'A')" > planted.ciso
		git add planted.ciso
		git commit -qm "plant game data"
		# clean file — none of this may be reported
		cat > clean.txt <<'CLEAN'
# nothing sensitive here
example=https://gateway.example.invalid/v1
localhost_ok=http://127.0.0.1:8080/health
public=example.com
short_key=sk-short123
public_ip=8.8.8.8:53
embedded=110.1.2.3 (a leading digit blocks the bare-IP boundary)
CLEAN
		git add clean.txt
		git commit -qm "clean file"
	) || return 2

	selfscan="$(cd "$(dirname -- "$0")" && pwd)/secret-scan.sh"
	(
		cd "$trepo" || exit 2
		exec bash "$selfscan" --history
	) >"$tmp/out.txt" 2>&1
	rc=$?
	out=$(cat "$tmp/out.txt" 2>/dev/null)

	fail=0
	check() { # $1 label, $2 1=present/0=absent, $3 substring
		local label=$1 want=$2 needle=$3
		if [ "$want" = 1 ]; then
			case $out in
			*"$needle"*) : ;;
			*) echo "self-test FAIL: $label — expected output missing: $needle" >&2; fail=1 ;;
			esac
		else
			case $out in
			*"$needle"*) echo "self-test FAIL: $label — unexpected output: $needle" >&2; fail=1 ;;
			esac
		fi
	}
	[ "$rc" = 1 ] || { echo "self-test FAIL: expected rc=1 (findings), got $rc" >&2; fail=1; }
	check "token-prefix sk- (len 30)" 1 'token-prefix [masked, len=30]'
	check "token-prefix ghp_ (len 29)" 1 'token-prefix [masked, len=29]'
	check "token-prefix xoxb- (len 24)" 1 'token-prefix [masked, len=24]'
	check "token-prefix AWS AKIA (len 20)" 1 'token-prefix [masked, len=20]'
	check "pem" 1 'pem [masked'
	check "jwt (len 68)" 1 'token-prefix [masked, len=68]'
	check "private-url RFC1918 (len 20)" 1 'private-url [masked, len=20]'
	check "home-path (len 18)" 1 'home-path [masked, len=18]'
	check "private-host user@ip (len 8)" 1 'private-host [masked, len=8]'
	check "private-host ip:port (len 11)" 1 'private-host [masked, len=11]'
	check "private-host not over-eager on public IP" 0 'public_ip'
	check "private-host not over-eager on embedded IP" 0 '110.1.2.3'
	# the .lan and .ts.net samples also match (each len 20): expect at least 3
	# private-url len-20 findings (RFC1918 + .lan + .ts.net)
	case $out in
	*'private-url [masked, len=20]'*)
		n=$(printf '%s\n' "$out" | grep -c 'private-url \[masked, len=20\]')
		[ "$n" -ge 3 ] || { echo "self-test FAIL: private-url — expected >=3 len-20 hits, got $n" >&2; fail=1; }
		;;
	*) echo "self-test FAIL: private-url — no len-20 hits at all" >&2; fail=1 ;;
esac
	check "game-data CISO" 1 'game-data'
	check "no high-entropy false positive on fixtures" 0 'high-entropy'
	check "clean file not flagged" 0 'clean.txt'
	check "localhost not flagged" 0 '127.0.0.1'
	check "no value leak (token body)" 0 'plantedselftest'
	check "no value leak (jwt signature)" 0 'planted_signature'
	check "no value leak (private-host ip)" 0 '10.1.2.3'
	check "no value leak (private-host ip2)" 0 '192.168.4.5'
	check "history mode saw the committed file" 1 'secrets.txt'
	if [ "$fail" = 0 ]; then
		echo "secret-scan: self-test PASS (all planted samples detected, clean file untouched, no value printed)"
		rm -rf "$tmp"
		trap - EXIT
		return 0
	fi
	rm -rf "$tmp"
	trap - EXIT
	return 1
}

# --- main ------------------------------------------------------------------------------
case ${1:-} in
'') : ;; # no args: scan the tracked tree
-h | --help) usage; exit 0 ;;
--history) HISTORY=1; shift ;;
--self-test) SELFTEST=1; shift ;;
--allow)
	ALLOW_OVERRIDE=${2:?--allow needs a path}
	shift 2
	;;
*) usage; exit 2 ;;
esac

if [ "$SELFTEST" = 1 ]; then
	self_test
	exit $?
fi

if ! command -v git >/dev/null 2>&1; then
	echo "NOT RUN: secret-scan (git not installed)" >&2
	exit 2
fi
if ! git rev-parse --is-inside-work-tree >/dev/null 2>&1; then
	echo "NOT RUN: secret-scan (not a git worktree)" >&2
	exit 2
fi
if [ -n "$ALLOW_OVERRIDE" ]; then
	ALLOW_FILE=$ALLOW_OVERRIDE
	[ -f "$ALLOW_FILE" ] || {
		echo "NOT RUN: secret-scan (allowlist $ALLOW_FILE not found)" >&2
		exit 2
	}
else
	ALLOW_FILE=$(git rev-parse --show-toplevel 2>/dev/null)/$DEFAULT_ALLOW
fi
if [ -f "$ALLOW_FILE" ]; then
	echo "secret-scan: allowlist: $ALLOW_FILE"
else
	echo "secret-scan: allowlist: (none)"
fi

scan_tree
scan_files
[ "$HISTORY" = 1 ] && scan_history

if [ -n "$ALLOWED_NOTES" ]; then
	echo "secret-scan: suppressed (allowlisted):"
	printf '%s' "$ALLOWED_NOTES"
fi

if [ "$FINDINGS" -gt 0 ]; then
	echo "secret-scan: $FINDINGS finding(s) — do not commit; fix or allowlist with a reason" >&2
	# per-finding ::error annotations were emitted above; the final summary
	# is a ::notice with the totals (annotate-go.sh convention)
	gha_notice "" "" "secret-scan: FAIL — $FINDINGS finding(s) — see the ::error annotations above"
	exit 1
fi
echo "secret-scan: clean"
gha_notice "" "" "secret-scan: PASS — 0 findings"
exit 0
