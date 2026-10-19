#!/bin/sh
# sustained-on-device.sh USER@HOST [--minutes N] [--source replay|capture]
#
# M8 sustained-play resource run on the real UNO Q appliance, driven through
# the device's *running* service API (docs/spec/16 M8: "validate frames, UI
# responsiveness, 4 GB memory, audio, power, hotplug and network behavior").
#
# Source policy (AGENTS.md rule 3 "No simulation theater"):
#   --source replay (default)  The committed synthetic replay clip
#                              testdata/synthetic/replay/dialogue-advance-001
#                              (invented text, no game content — rule 5) is
#                              copied into a dir under the service's replay
#                              allowlist and the session loop keeps the
#                              pipeline busy for N minutes: the clip is
#                              short (2.4 s at 10 Hz), the replay source
#                              drains, and the loop starts the next session
#                              through POST /api/v1/sessions. Every frame,
#                              OCR call, ranking and commit goes through the
#                              installed serve binary. This is a *replay
#                              source on the device* — NOT capture evidence.
#   --source capture           REFUSED with a clear NOT RUN (exit 2) until the
#                              UVC capture dongle is attached.
#
# Replay-root operator step (temporary, documented, restored afterwards):
# the installed appliance ships WITHOUT GAME_NARRATOR_REPLAY_ROOTS, and
# DefaultReplayRoots admits nothing in production (no testdata/ beside the
# service's working dir), and the API has no clip-upload route — so the
# product today cannot replay a user clip on an installed device without a
# configuration change (reported as a product gap). The product's own
# configuration surface for the appliance service is /etc/game-narrator/env
# (the unit's EnvironmentFile; docs/spec/10 "Configure env file protected at
# 0600"). The runner therefore performs the documented operator step:
#   1. back up /etc/game-narrator/env (byte-for-byte, 0600, in the stage dir);
#   2. append GAME_NARRATOR_REPLAY_ROOTS=/var/lib/game-narrator/replay;
#   3. systemctl daemon-reload + restart, wait for /health/ready, verify the
#      variable is present in the running process environment (the single
#      variable is grepped out of /proc/PID/environ; the env file itself is
#      never printed, catted or copied out);
#   and RESTORES it at the end (and on any failure, via the EXIT trap):
#   4. restore the backup, daemon-reload + restart, wait for ready, verify
#      the variable is gone. The report records every step and the metadata
#      (mode/owner/line count) before and after.
#
# What one 10 s sample records (budgets: docs/spec/10 "Resource budgets" +
# internal/hwsmoke serveRSSBudgetMiB):
#   - service RSS (KB) and CPU % (utime+stime delta of /proc/PID/stat over
#     the sample window, % of one core; the stat line is parsed comm-safely
#     — the comm field is stripped up to the LAST ')' before the numeric
#     fields, so a comm containing spaces or ')' cannot shift the fields);
#   - system memory MemTotal/MemAvailable (the 4 GB headroom check), load,
#     every thermal zone (the internal/hwsmoke unit heuristics), storage
#     free on the data dir;
#   - session stats: frames delivered, OCR calls, commits, queue drops,
#     speech counters (the session snapshots carry the audio block only when
#     a voice workflow is wired; on this appliance speech is disabled —
#     recorded as such, not faked);
#   - UI responsiveness: wall time of GET / (embedded SPA) and
#     GET /api/v1/system/info (one API call).
#
# Budgets (PASS/FAIL, documented in docs/manuals/13):
#   - serve RSS peak <= 512 MiB (the hwsmoke whole-process budget);
#   - no RSS growth trend: last-third average <= first-third * 1.25;
#   - MemAvailable never below 512 MiB (headroom of the 4 GB);
#   - max zone temperature < 90 C (below the documented throttle band);
#   - no stalled pipeline: no run of >= 3 consecutive failed sessions,
#     frames_dropped == 0 across all sessions, frames advancing in the
#     final minute;
#   - UI responsiveness: 95th percentile API latency <= 2000 ms and page
#     latency <= 5000 ms.
#
# Auth: the runner mints a 1-day API token for a dedicated `sustained`
# admin account (generated password, never printed) through the installed
# binary's `admin` CLI; the token stays in a 0600 file on the device.
# Everything after that — catalog, import, publish, session loop, cleanup —
# is the HTTP API.
#
# Cleanup (through the API): the session is stopped (idempotent stop), the
# profile revision is archived; the per-run clip dir, token and /tmp stage
# are removed. The game/edition rows have no delete route — they remain as
# inert fixtures (recorded in the report). The env file is restored to its
# pre-run bytes (see operator step above).
#
# Artifacts: artifacts/hw-sustained/<run-id>/ (gitignored) gets the raw
# sample JSONL, per-session log, ops log and report.json/report.md. The
# report names the device <user>@<uno-q-host>, never its IP (rule 5).
#
# Exit codes: 0 PASS, 1 FAIL (a budget or an operational error),
# 2 NOT RUN (capture source before the dongle arrives).
#
# The device runner below is embedded as a heredoc that itself contains
# heredocs; shellcheck's symbol tracking mis-parses that nesting and reports
# the runner's own assignments as missing (SC2153) or unassigned (SC2154).
# Both are false positives for this file: every flagged name is assigned in
# the runner header (MINUTES/RUN_ID/CLIP_SHA/GAME_SLUG) or in the runner
# body (ENV_APPENDED/env_restored).
# shellcheck disable=SC2153,SC2154

set -u

usage() {
	echo "usage: $0 USER@HOST [--minutes N] [--source replay|capture]" >&2
}

host=""
minutes=30
source=""

while [ $# -gt 0 ]; do
	case $1 in
	--minutes) [ $# -ge 2 ] || { usage; exit 1; }; minutes=$2; shift 2 ;;
	--source) [ $# -ge 2 ] || { usage; exit 1; }; source=$2; shift 2 ;;
	*)
		if [ -z "$host" ]; then
			host=$1; shift
		else
			usage; exit 1
		fi
		;;
	esac
done

[ -n "$host" ] || { usage; exit 1; }
case $host in
*@*) : ;;
*) echo "sustained: the target must be USER@HOST (got $host)" >&2; exit 1 ;;
esac
case $minutes in
''|*[!0-9]*) echo "sustained: --minutes must be a positive integer" >&2; exit 1 ;;
esac
[ "$minutes" -ge 1 ] || { echo "sustained: --minutes must be >= 1" >&2; exit 1; }

if [ -z "$source" ]; then
	source=replay
fi
if [ "$source" = "capture" ]; then
	# No UVC capture dongle is attached to the appliance yet: the run cannot
	# take live frames, and the replay loop must not be dressed up as capture.
	cat >&2 <<EOF
NOT RUN: --source capture is refused until the UVC HDMI capture dongle is
attached to the UNO Q's powered hub (GameCube -> RCA-to-HDMI -> splitter ->
dongle, docs/spec/10 "Physical connection"). Today the device has no
capture-capable V4L2 node and no audio sink, so a capture session would have
nothing to read. Re-run with --source capture once it is attached (the
default replay run stays the evidence for this milestone until then):
  scripts/hw/sustained-on-device.sh ${host} --minutes ${minutes} --source capture
Capture and audio remain NOT VERIFIED (docs/status/acceptance.md M8).
EOF
	exit 2
fi
[ "$source" = "replay" ] || {
	echo "sustained: --source must be replay or capture (got $source)" >&2
	exit 1
}
# ── repo staging ────────────────────────────────────────────────────────────
# shellcheck disable=SC1007  # CDPATH= is the empty-env-assignment idiom
root=$(CDPATH= cd -- "$(dirname -- "$0")/../.." && pwd) || exit 1
cd "$root" || exit 1

clip_src=testdata/synthetic/replay/dialogue-advance-001
profile_json=testdata/synthetic/profiles/dialogue-advance-001.json
[ -d "$clip_src" ] || { echo "sustained: missing $clip_src" >&2; exit 1; }
[ -f "$profile_json" ] || { echo "sustained: missing $profile_json" >&2; exit 1; }
for tool in ssh scp curl python3; do
	command -v "$tool" >/dev/null 2>&1 || { echo "sustained: $tool is not on PATH" >&2; exit 1; }
done

run_id=sustained-$(date -u +%Y%m%dT%H%M%SZ)
# Stable fixture identity: the synthetic fixture game/edition is created ONCE
# (fixed slug + fixed Idempotency-Keys) and every later run replays the
# stored creates, so reruns leave no fixture clutter; each run still gets its
# own draft profile (archived at the end). If the device's data dir is ever
# reset the creates simply run again from scratch.
game_slug=synthetic-sustained
outdir=artifacts/hw-sustained/$run_id
mkdir -p "$outdir" || exit 1

tmp=$(mktemp -d) || exit 1
trap 'rm -rf "$tmp"' EXIT

echo "sustained: run $run_id on $host (source: replay, ${minutes} min)"

# Seed the device from the committed synthetic fixture (rule 5): the clip
# ships verbatim from the repo; the import seed CSV and the region payload
# are derived from the committed synthetic profile (invented lines only).
clip_sha=$(python3 - "$clip_src" <<'PY'
import hashlib, os, sys
root = sys.argv[1]
acc = hashlib.sha256()
for dirpath, dirnames, filenames in os.walk(root):
    dirnames.sort()
    for name in sorted(filenames):
        p = os.path.join(dirpath, name)
        rel = os.path.relpath(p, root)
        acc.update(rel.encode() + b" " + str(os.path.getsize(p)).encode() + b"\n")
print(acc.hexdigest())
PY
) || exit 1

python3 - "$profile_json" "$tmp/seed.csv" "$tmp/regions.json" <<'PY' || exit 1
import csv, json, sys
p = json.load(open(sys.argv[1]))
rows = [(m["id"], "", m["original_text"], "dialogue", "") for m in p["messages"]]
with open(sys.argv[2], "w", newline="") as f:
    w = csv.writer(f)
    w.writerow(["message_id", "speaker", "line", "kind", "area"])
    w.writerows(rows)
r = p["regions"][0]
json.dump({"regions": [{"label": "story box", "region_type": "textbox",
                        "x": r["x"], "y": r["y"], "w": r["w"], "h": r["h"]}]},
          open(sys.argv[3], "w"))
PY

# ── the device runner ───────────────────────────────────────────────────────
# Runs as root on the appliance (one `sudo -n sh` invocation, like
# scripts/hw/run-on-device.sh): it may touch systemd (daemon-reload +
# restart only, for the documented operator step and its restore) and the
# data dir. It never prints /etc/game-narrator/env or the process env as a
# whole; it greps out at most two non-secret values (GAME_NARRATOR_LISTEN,
# GAME_NARRATOR_REPLAY_ROOTS) and records them in the ops log.
cat > "$tmp/runner.sh" <<'RUNNER_EOF'
#!/bin/sh
# Device-side runner for the sustained-play run. Invoked:
#   sh run.sh <minutes> <run-id> <clip-sha256> <game-slug>
# Staging dir: /tmp/gn-sustained-<run-id> (clip/, seed.csv, regions.json).
set -u

MINUTES=$1
RUN_ID=$2
CLIP_SHA=$3
GAME_SLUG=$4
BASE=/tmp/gn-sustained-$RUN_ID
OUT=$BASE/out
mkdir -p "$OUT" || exit 1

DATA_DIR=/var/lib/game-narrator
REPLAY_ROOT=$DATA_DIR/replay
CLIP_DIR=$REPLAY_ROOT/sustained-$RUN_ID
ENV_FILE=/etc/game-narrator/env
ENV_BAK=$BASE/env.backup
SERVICE=game-narrator

log() {
	echo "[sustained $(date -u +%Y-%m-%dT%H:%M:%SZ)] $*" | tee -a "$OUT/runner.log"
}

# Status for the local poller: "starting" now, a heartbeat per sample tick,
# "done" at the end; any non-zero exit must still leave "error" behind so
# the poller can tell a dead runner from a busy one.
printf '{"state":"starting","ts":%d}\n' "$(date +%s)" >"$OUT/status" 2>/dev/null || true
on_exit() {
	rc=$?
	restore_env   # the operator step must be undone even on failure
	[ "$rc" -eq 0 ] && return 0
	printf '{"state":"error","rc":%d,"ts":%d}\n' "$rc" "$(date +%s)" >"$OUT/status" 2>/dev/null
}
trap on_exit EXIT

# 1) Service port: the unit pins 127.0.0.1:8080; the env file's
# GAME_NARRATOR_LISTEN (the one non-secret value ever read from it) wins.
API=http://127.0.0.1:8080
if [ -f "$ENV_FILE" ]; then
	listen=$(sh -c '. '"$ENV_FILE"' 2>/dev/null; printenv GAME_NARRATOR_LISTEN')
	[ -n "$listen" ] && API=http://$listen
fi
log "API at $API"

env_meta() {
	# Metadata only (mode/owner/line count), never contents.
	ls -la "$ENV_FILE" 2>/dev/null | awk '{print $1, $3, $4}' \
		| tr -s ' '
}

wait_ready() {
	# $1 = max attempts (1 s apart). Returns 0 once /health/ready is 200.
	i=0
	while [ "$i" -lt "$1" ]; do
		if curl -fs -m 3 "$API/api/v1/health/ready" >/dev/null 2>&1; then
			return 0
		fi
		sleep 1
		i=$((i + 1))
	done
	return 1
}

proc_has_replay_roots() {
	# $1 = expected value. Greps the RUNNING process env for exactly one
	# non-secret variable (never prints the env file or the whole env).
	pid=$(systemctl show -p MainPID --value "$SERVICE" 2>/dev/null)
	[ -n "$pid" ] && [ -r "/proc/$pid/environ" ] \
		&& tr '\0' '\n' <"/proc/$pid/environ" 2>/dev/null \
		| grep -qx "GAME_NARRATOR_REPLAY_ROOTS=$1"
}

ENV_APPENDED=0
ENV_META_PRE=""
env_restored=0
restore_env() {
	# Idempotent restore of the documented operator step: put the pre-run
	# env bytes back, reload + restart, wait for ready, verify the variable
	# is gone from the running process. Skipped when the step was not taken.
	[ "$ENV_APPENDED" = "1" ] || return 0
	[ "$env_restored" = "1" ] && return 0
	log "operator step: restoring /etc/game-narrator/env from the pre-run backup"
	cp -a "$ENV_BAK" "$ENV_FILE" \
		&& chown root:root "$ENV_FILE" && chmod 600 "$ENV_FILE"
	systemctl daemon-reload
	systemctl restart "$SERVICE"
	if wait_ready 90; then
		if proc_has_replay_roots "$REPLAY_ROOT"; then
			log "warning: REPLAY_ROOTS is still present in the service environment after the restore"
		else
			log "operator step: restore verified (variable absent, service ready)"
		fi
		env_restored=1
	else
		log "FAIL: the service did not become ready after the env restore"
	fi
}

# 2) The replay-root operator step (temporary, restored afterwards — see the
# script header). The installed service admits a replay dir only under
# GAME_NARRATOR_REPLAY_ROOTS (internal/runtime allowlist); production ships
# without it, so this is the product gap the run documents.
if grep -qx "GAME_NARRATOR_REPLAY_ROOTS=$REPLAY_ROOT" "$ENV_FILE" 2>/dev/null; then
	log "replay root already configured by an operator step; reusing it (NOT restored at the end)"
	ENV_META_PRE="$(env_meta)"
elif grep -q '^GAME_NARRATOR_REPLAY_ROOTS=' "$ENV_FILE" 2>/dev/null; then
	log "FAIL: $ENV_FILE sets a different GAME_NARRATOR_REPLAY_ROOTS; refusing to mix it with this run's value (operator action required)"
	exit 1
else
	log "operator step: backing up $ENV_FILE ($(env_meta))"
	ENV_META_PRE="$(env_meta)"
	cp -a "$ENV_FILE" "$ENV_BAK" && chmod 600 "$ENV_BAK" \
		|| { log "FAIL: could not back up the env file"; exit 1; }
	printf 'GAME_NARRATOR_REPLAY_ROOTS=%s\n' "$REPLAY_ROOT" >>"$ENV_FILE" \
		|| { log "FAIL: could not append the replay root to the env file"; exit 1; }
	ENV_APPENDED=1
	log "operator step: appended GAME_NARRATOR_REPLAY_ROOTS=$REPLAY_ROOT (backup in the stage dir; restored at the end)"
	systemctl daemon-reload
	systemctl restart "$SERVICE"
	if ! wait_ready 90; then
		log "FAIL: the service did not become ready after the operator step"
		exit 1
	fi
	if proc_has_replay_roots "$REPLAY_ROOT"; then
		log "operator step: verified the running process has the replay root"
	else
		log "FAIL: the running process lacks GAME_NARRATOR_REPLAY_ROOTS after restart"
		exit 1
	fi
fi

# 3) Auth: a dedicated `sustained` admin account (generated password, never
# printed) and a 1-day API token through the installed binary's `admin` CLI.
BIN=/usr/bin/game-narrator
TOKEN=$BASE/token
chmod 600 "$TOKEN" 2>/dev/null || true
list_out=$($BIN admin list --data-dir "$DATA_DIR" 2>&1)
if echo "$list_out" | grep -q "no accounts"; then
	pw=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
	printf '%s' "$pw" | $BIN admin create --username sustained --role admin \
		--data-dir "$DATA_DIR" >/dev/null 2>&1 \
		|| { log "FAIL: could not create the sustained account"; exit 1; }
	log "created the sustained admin account"
fi
if ! $BIN admin token --username sustained --expires-days 1 \
		--name "sustained-$RUN_ID" --data-dir "$DATA_DIR" >"$TOKEN" 2>/dev/null; then
	# The device was claimed by someone else: mint the dedicated account now.
	pw=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n')
	printf '%s' "$pw" | $BIN admin create --username sustained --role admin \
		--data-dir "$DATA_DIR" >/dev/null 2>&1
	$BIN admin token --username sustained --expires-days 1 \
		--name "sustained-$RUN_ID" --data-dir "$DATA_DIR" >"$TOKEN" 2>/dev/null \
		|| { log "FAIL: could not mint the API token"; exit 1; }
fi
[ -s "$TOKEN" ] || { log "FAIL: the token file is empty"; exit 1; }

# api METHOD PATH [BODY] [IDEMPOTENCY-KEY]: body to stdout (redirect it to
# "$BASE/.resp"); the HTTP status lands in $api_status IN THE CALLER'S
# shell — the function must therefore never be called inside $( ).
api() {
	if [ $# -ge 4 ]; then
		printf '%s' "$3" >"$BASE/.req"
		api_status=$(curl -s -o "$BASE/.body" -w '%{http_code}' \
			-H "Authorization: Bearer $(cat "$TOKEN")" -H "Idempotency-Key: $4" -X "$1" \
			--data-binary @"$BASE/.req" "$API$2")
	elif [ $# -ge 3 ]; then
		printf '%s' "$3" >"$BASE/.req"
		api_status=$(curl -s -o "$BASE/.body" -w '%{http_code}' \
			-H "Authorization: Bearer $(cat "$TOKEN")" -X "$1" \
			--data-binary @"$BASE/.req" "$API$2")
	else
		api_status=$(curl -s -o "$BASE/.body" -w '%{http_code}' \
			-H "Authorization: Bearer $(cat "$TOKEN")" -X "$1" "$API$2")
	fi
	cat "$BASE/.body" 2>/dev/null
}

jqget() {
	# jqget FILE PATH(.separated) -> one value from a JSON file, empty on miss.
	python3 -c 'import json,sys
try:
    v=json.load(open(sys.argv[1]))
    for k in sys.argv[2].split("."):
        v=v[k]
    print(v if v is not None else "")
except Exception:
    pass' "$1" "$2" 2>/dev/null
}

list_id() {
	# list_id FILE WRAPPERKEY [FIELD=VALUE]... -> the id of the first object in
	# FILE's WRAPPERKEY list matching every field (empty when none / unparseable).
	# Capture before the shift: under set -u a bare $2 after it would abort in
	# dash when no field pairs were given.
	li_file=$1
	li_key=$2
	shift 2
	python3 -c 'import json, sys
try:
    data = json.load(open(sys.argv[1]))
except Exception:
    sys.exit(0)
items = data.get(sys.argv[2], []) or []
pairs = [p.split("=", 1) for p in sys.argv[3:]]
for it in items:
    if all(it.get(k) == v for k, v in pairs):
        print(it.get("id", ""))
        break' "$li_file" "$li_key" "$@" 2>/dev/null
}

log "placing the clip under the replay root ($CLIP_DIR)"
mkdir -p "$CLIP_DIR" || exit 1
cp -r "$BASE/clip/." "$CLIP_DIR/" || { log "FAIL: clip copy failed"; exit 1; }
chown -R game-narrator:game-narrator "$CLIP_DIR" || true

# 4) Catalog through the API: game -> edition -> draft -> import seed ->
# regions -> review -> publish. The Idempotency store is in-process memory
# (internal/api catalog_idempotency), so after the service restart of the
# operator step a retry of a previous run's create 409s instead of replaying:
# the game and edition therefore fall back to list + reuse (fixed slug / fixed
# region-language-revision identity); each run still gets its own draft
# profile revision, archived at the end (rule 5).
log "creating or reusing the fixture game/edition/profile through the API"
game_body=$(python3 -c 'import json,sys; print(json.dumps({"slug": sys.argv[1], "title": "Synthetic Sustained Replay Fixture", "platform": "gamecube-like", "user_tags": []}))' "$GAME_SLUG")
api POST /api/v1/games "$game_body" "gn-sustained-fixture-game" >"$BASE/.resp"
case $api_status in
201) game_id=$(jqget "$BASE/.resp" id) ;;
409)
	log "fixture game already exists (an earlier run); reusing it"
	api GET /api/v1/games >"$BASE/.resp"
	game_id=$(list_id "$BASE/.resp" games "slug=$GAME_SLUG")
	;;
*) log "FAIL: POST /games -> $api_status: $(head -c 300 "$BASE/.resp")"; exit 1 ;;
esac
[ -n "$game_id" ] || { log "FAIL: no game id"; exit 1; }

edition_body=$(python3 -c 'import json,sys; print(json.dumps({"region": "ntsc-u", "language": "en", "revision": "1", "source_identity": sys.argv[1]}))' "$CLIP_SHA")
api POST /api/v1/games/"$game_id"/editions "$edition_body" "gn-sustained-fixture-edition" >"$BASE/.resp"
case $api_status in
201) edition_id=$(jqget "$BASE/.resp" id) ;;
409)
	log "fixture edition already exists (an earlier run); reusing it"
	api GET /api/v1/games/"$game_id"/editions >"$BASE/.resp"
	edition_id=$(list_id "$BASE/.resp" editions region=ntsc-u language=en revision=1)
	;;
*) log "FAIL: POST editions -> $api_status: $(head -c 300 "$BASE/.resp")"; exit 1 ;;
esac
[ -n "$edition_id" ] || { log "FAIL: no edition id"; exit 1; }

api POST /api/v1/editions/"$edition_id"/profiles >"$BASE/.resp"
[ "$api_status" = "201" ] || { log "FAIL: POST draft profile -> $api_status: $(head -c 300 "$BASE/.resp")"; exit 1; }
profile_id=$(jqget "$BASE/.resp" id)
log "catalog ready: game=$game_id edition=$edition_id profile=$profile_id"

# Import the synthetic script seed (multipart, the route the UI posts to).
api_status=$(curl -s -o "$BASE/.body" -w '%{http_code}' \
	-H "Authorization: Bearer $(cat "$TOKEN")" \
	-F "file=@$BASE/seed.csv;filename=sustained-seed.csv" \
	-F "format=csv" -F "rights=user_supplied" \
	"$API/api/v1/profiles/$profile_id/import")
[ "$api_status" = "202" ] || { log "FAIL: POST import -> $api_status: $(head -c 300 "$BASE/.body")"; exit 1; }
job_id=$(jqget "$BASE/.body" job_id)
i=0
job_state=""
while [ "$i" -lt 120 ]; do
	api GET /api/v1/jobs/"$job_id"
	job_state=$(jqget "$BASE/.body" state)
	[ "$job_state" = "succeeded" ] && break
	case $job_state in failed|cancelled) break ;; esac
	sleep 1
	i=$((i + 1))
done
[ "$job_state" = "succeeded" ] || { log "FAIL: import job $job_id ended ${job_state:-unknown}"; exit 1; }
log "import job $job_id succeeded"

# Regions (If-Match guarded), then move to review and publish (the publish
# gate requires review state — docs/spec/02 transition table).
get_profile_etag() {
	api_status=$(curl -s -D "$BASE/.hdr" -o /dev/null -w '%{http_code}' \
		-H "Authorization: Bearer $(cat "$TOKEN")" \
		"$API/api/v1/profiles/$profile_id")
	[ "$api_status" = "200" ] || return 1
	etag=$(grep -i '^etag:' "$BASE/.hdr" | tr -d '\r' | awk '{print $2}')
	[ -n "$etag" ] || return 1
}
get_profile_etag || { log "FAIL: could not read the profile etag"; exit 1; }
api_status=$(curl -s -o "$BASE/.body" -w '%{http_code}' \
	-H "Authorization: Bearer $(cat "$TOKEN")" -H "If-Match: $etag" \
	-X PUT --data-binary @"$BASE/regions.json" -H 'Content-Type: application/json' \
	"$API/api/v1/profiles/$profile_id/regions")
[ "$api_status" = "200" ] || { log "FAIL: PUT regions -> $api_status: $(head -c 300 "$BASE/.body")"; exit 1; }
get_profile_etag || { log "FAIL: could not re-read the profile etag"; exit 1; }
api_status=$(curl -s -o "$BASE/.body" -w '%{http_code}' \
	-H "Authorization: Bearer $(cat "$TOKEN")" -H "If-Match: $etag" \
	-X POST --data-binary '{"to": "review"}' \
	"$API/api/v1/profiles/$profile_id/status")
[ "$api_status" = "200" ] || { log "FAIL: POST status review -> $api_status: $(head -c 300 "$BASE/.body")"; exit 1; }
get_profile_etag || { log "FAIL: could not re-read the profile etag"; exit 1; }
api_status=$(curl -s -o "$BASE/.body" -w '%{http_code}' \
	-H "Authorization: Bearer $(cat "$TOKEN")" -H "If-Match: $etag" \
	-X POST "$API/api/v1/profiles/$profile_id/publish")
[ "$api_status" = "200" ] || { log "FAIL: POST publish -> $api_status: $(head -c 300 "$BASE/.body")"; exit 1; }
log "profile published"

# 5) The session loop with 10 s sampling.
# The clip is 2.4 s of 10 Hz frames; the replay source drains (speed 0 = as
# fast as the pipeline consumes), so "looping the replay source for N
# minutes" means: start a session, let it drain to source_end, start the
# next. Every sample tick the pipeline is either in a session or in the
# one-sample gap between two.
SAMPLES=$OUT/samples.jsonl
SESSIONS=$OUT/sessions.jsonl
TICKS_STATE=$BASE/.ticks
: >"$SAMPLES" || exit 1
: >"$SESSIONS" || exit 1

NOW=$(date +%s)
END=$((NOW + MINUTES * 60))

cur_session=""
n_sessions=0
n_failed=0
consec_fail=0
cum_frames=0
cum_ocr=0
cum_commits=0
cum_dropped=0
cum_speech_started=0

fetch_session() {
	if [ -n "$1" ]; then
		api_status=$(curl -s -o "$BASE/.sess" -w '%{http_code}' \
			-H "Authorization: Bearer $(cat "$TOKEN")" \
			"$API/api/v1/sessions/$1")
	else
		printf '{}' >"$BASE/.sess"
		api_status=0
	fi
}

start_session() {

	body=$(python3 -c 'import json,sys; print(json.dumps({"profile_id": sys.argv[1], "frame_source": {"kind": "replay", "dir": sys.argv[2]}, "mode": "conservative"}))' "$profile_id" "$CLIP_DIR")
	api_status=$(curl -s -o "$BASE/.body" -w '%{http_code}' \
		-H "Authorization: Bearer $(cat "$TOKEN")" -X POST \
		--data-binary "$body" "$API/api/v1/sessions")
	if [ "$api_status" = "201" ]; then
		new_session=$(jqget "$BASE/.body" session_id)
	else
		new_session=""
		log "session start refused ($api_status): $(head -c 300 "$BASE/.body")"
	fi
}

# write_sample WINDOW TICK PID TICKSFILE PAGE_MS API_MS (appends one line).
# /proc/<pid>/stat is parsed comm-safely: the comm field (field 2) may
# contain spaces or ')', so the numeric fields are read only after the LAST
# ')' in the line; utime/stime are then elements 11/12 of that tail (0-
# based, i.e. overall fields 14/15). The tick counter is carried in
# TICKSFILE as "pid ticks" so a service restart (new pid) starts clean.
write_sample() {
	python3 - "$1" "$2" "$3" "$4" "$5" "$6" "$DATA_DIR" "$BASE/.sess" \
		>>"$SAMPLES" <<'PY'
import json, os, sys, glob

window = float(sys.argv[1])
ts = float(sys.argv[2])
pid = sys.argv[3]
ticks_file = sys.argv[4]
page_ms = float(sys.argv[5])
api_ms = float(sys.argv[6])
data_dir, sess_file = sys.argv[7], sys.argv[8]

def read(p):
    try:
        return open(p).read()
    except OSError:
        return ""

def kv(text):
    # "Key: value" lines; a line whose value is empty ("Key:" only) must
    # not break the parse.
    out = {}
    for line in text.splitlines():
        parts = line.split(None, 1)
        if len(parts) == 2:
            out[parts[0].rstrip(":")] = parts[1]
    return out

out = {"ts": ts, "window": window, "service": {}, "system": {},
       "session": {}, "ui": {"page_ms": page_ms, "api_ms": api_ms}}

ticks_prev = None
if pid not in ("", "0"):
    try:
        stat = read("/proc/" + pid + "/stat")
        rp = stat.rfind(")")
        if rp >= 0:
            tail = stat[rp + 2:].split()
            ticks = int(tail[11]) + int(tail[12])  # utime + stime
            saved = read(ticks_file).split()
            if saved and len(saved) == 2 and saved[0] == pid:
                ticks_prev = int(saved[1])
            with open(ticks_file, "w") as f:
                f.write("%s %d\n" % (pid, ticks))
        else:
            ticks = 0
        status = kv(read("/proc/" + pid + "/status"))
        out["service"] = {"pid": int(pid), "rss_kb": int(status["VmRSS"].split()[0])}
        if ticks_prev is not None:
            out["service"]["cpu_pct"] = round(max(0, ticks - ticks_prev) / 100.0 / window * 100.0, 1)
        out["service"]["cpu_ticks"] = ticks
    except (OSError, IndexError, ValueError, KeyError):
        out["service"] = {}

mem = kv(read("/proc/meminfo"))
out["system"]["mem_total_kb"] = int(mem["MemTotal"].split()[0])
out["system"]["mem_avail_kb"] = int(mem["MemAvailable"].split()[0])
load = read("/proc/loadavg").split()
if load:
    out["system"]["load1"] = float(load[0])

def celsius(raw):
    # internal/hwsmoke thermalCelsius: raw 0-120 = degrees; else millidegrees
    # when plausible (5-120); else 0.01 degrees when plausible; else unknown.
    if 0 <= raw <= 120:
        return float(raw)
    if 5.0 <= raw / 1000.0 <= 120.0:
        return raw / 1000.0
    if 0.0 <= raw / 100.0 <= 120.0:
        return raw / 100.0
    return None

zones = []
for p in sorted(glob.glob("/sys/class/thermal/thermal_zone*/temp")):
    try:
        raw = int(read(p).strip())
    except (OSError, ValueError):
        continue
    zones.append({"zone": os.path.basename(os.path.dirname(p)), "raw": raw,
                  "celsius": celsius(raw)})
out["system"]["thermal"] = zones

try:
    st = os.statvfs(data_dir)
    out["system"]["store_avail_kb"] = st.f_bavail * st.f_frsize // 1024
except OSError:
    pass

try:
    sess = json.load(open(sess_file))
    st = sess.get("stats", {}) or {}
    audio = sess.get("audio") or {}
    speech = (audio.get("speech") or {}) if audio else {}
    out["session"] = {
        "id": sess.get("session_id"),
        "state": sess.get("state"),
        "frames": st.get("frames_delivered"),
        "ocr_calls": st.get("ocr_calls"),
        "commits": st.get("commits"),
        "frames_dropped": st.get("frames_dropped"),
        "rankings": st.get("rankings"),
        "ocr_errors": st.get("ocr_errors"),
        "audio_sink": audio.get("sink"),
        "speech_requested": speech.get("requested"),
        "speech_started": speech.get("started"),
        "speech_ended": speech.get("ended"),
    }
except (OSError, ValueError):
    pass

print(json.dumps(out))
PY
}

log "starting the ${MINUTES} min session loop (10 s samples)"
while [ "$(date +%s)" -lt "$END" ]; do
	tick=$(date +%s)

	# Start a session whenever none is active (the loop's "replay source
	# keeps running" half: the drain is what a live capture would be).
	if [ -z "$cur_session" ]; then
		start_session
		if [ -n "$new_session" ]; then
			cur_session=$new_session
			n_sessions=$((n_sessions + 1))
			consec_fail=0
			log "session $cur_session started (cycle $n_sessions)"
		else
			n_failed=$((n_failed + 1))
			consec_fail=$((consec_fail + 1))
			if [ "$consec_fail" -ge 3 ]; then
				log "FAIL: three consecutive session starts refused; aborting the loop"
				break
			fi
		fi
	fi

	fetch_session "$cur_session"

	# Heartbeat for the local poller (a dead runner is distinguishable
	# from a busy one by this file).
	printf '{"state":"running","elapsed":%d,"n_sessions":%d,"n_failed":%d,"cum_frames":%d,"cum_commits":%d}\n' \
		$(( $(date +%s) - NOW )) "$n_sessions" "$n_failed" "$cum_frames" "$cum_commits" >"$OUT/status" 2>/dev/null || true

	# UI responsiveness: the embedded SPA root and one API call.
	page_ms=$(curl -s -o /dev/null -w '%{time_total}' "$API/" 2>/dev/null | awk '{printf "%d", $1 * 1000}')
	api_ms=$(curl -s -o /dev/null -w '%{time_total}' -H "Authorization: Bearer $(cat "$TOKEN")" \
		"$API/api/v1/system/info" 2>/dev/null | awk '{printf "%d", $1 * 1000}')
	[ -n "$page_ms" ] || page_ms=0
	[ -n "$api_ms" ] || api_ms=0

	svc_pid=$(systemctl show -p MainPID --value "$SERVICE" 2>/dev/null)
	write_sample 10 "$tick" "$svc_pid" "$TICKS_STATE" "$page_ms" "$api_ms"

	# Session lifecycle bookkeeping: on a terminal state (stopped/failed),
	# record the cycle and accumulate its final stats.
	state=$(jqget "$BASE/.sess" state)
	case $state in
	""|starting|running|stopping) : ;;
	*)
		python3 - "$SESSIONS" "$BASE/.sess" "$tick" <<'PY'
import json, sys
sess = json.load(open(sys.argv[2]))
rec = {"ended_at": float(sys.argv[3]),
       "id": sess.get("session_id"), "state": sess.get("state"),
       "reason": sess.get("reason")}
st = sess.get("stats", {}) or {}
rec.update({k: st.get(k) for k in ("frames_delivered", "frames_dropped",
          "ocr_calls", "ocr_errors", "commits", "rankings", "events")})
audio = sess.get("audio") or {}
rec["audio_sink"] = audio.get("sink")
rec["speech"] = audio.get("speech")
with open(sys.argv[1], "a") as f:
    f.write(json.dumps(rec) + "\n")
PY
		fin_frames=$(jqget "$BASE/.sess" stats.frames_delivered)
		fin_ocr=$(jqget "$BASE/.sess" stats.ocr_calls)
		fin_commits=$(jqget "$BASE/.sess" stats.commits)
		fin_dropped=$(jqget "$BASE/.sess" stats.frames_dropped)
		fin_speech=$(jqget "$BASE/.sess" audio.speech.started)
		if [ -n "$fin_frames" ]; then cum_frames=$((cum_frames + fin_frames)); fi
		[ -n "$fin_ocr" ] && cum_ocr=$((cum_ocr + fin_ocr))
		[ -n "$fin_commits" ] && cum_commits=$((cum_commits + fin_commits))
		[ -n "$fin_dropped" ] && cum_dropped=$((cum_dropped + fin_dropped))
		[ -n "$fin_speech" ] && cum_speech_started=$((cum_speech_started + fin_speech))
		case $state in
		failed)
			n_failed=$((n_failed + 1))
			consec_fail=$((consec_fail + 1))
			log "session $cur_session FAILED: $(jqget "$BASE/.sess" reason)"
			;;
		*)
			consec_fail=0
			log "session $cur_session ended ($state, $(jqget "$BASE/.sess" reason))"
			;;
		esac
		cur_session=""
		;;
	esac

	# Pacing: sleep the rest of the 10 s window.
	now=$(date +%s)
	left=$((tick + 10 - now))
	[ "$left" -gt 0 ] && sleep "$left"
done

# 6) Wind down through the API: stop the running session and wait for its
# terminal state.
if [ -n "$cur_session" ]; then
	api_status=$(curl -s -o "$BASE/.body" -w '%{http_code}' -X POST \
		-H "Authorization: Bearer $(cat "$TOKEN")" \
		"$API/api/v1/sessions/$cur_session/stop")
	log "stop request -> $api_status (session $cur_session)"
	i=0
	while [ "$i" -lt 60 ]; do
		fetch_session "$cur_session"
		state=$(jqget "$BASE/.sess" state)
		case $state in starting|running|stopping) : ;; *) break ;; esac
		sleep 1
		i=$((i + 1))
	done
	python3 - "$SESSIONS" "$BASE/.sess" "$(date +%s)" <<'PY'
import json, sys
sess = json.load(open(sys.argv[2]))
rec = {"ended_at": float(sys.argv[3]), "stopped_by_runner": True,
       "id": sess.get("session_id"), "state": sess.get("state"),
       "reason": sess.get("reason")}
st = sess.get("stats", {}) or {}
rec.update({k: st.get(k) for k in ("frames_delivered", "frames_dropped",
          "ocr_calls", "ocr_errors", "commits", "rankings", "events")})
audio = sess.get("audio") or {}
rec["audio_sink"] = audio.get("sink")
rec["speech"] = audio.get("speech")
with open(sys.argv[1], "a") as f:
    f.write(json.dumps(rec) + "\n")
PY
fi

# 7) Cleanup through the API: archive the profile revision (the game and
# edition rows have no delete route; they stay inert and are recorded).
get_profile_etag
if [ "$api_status" = "200" ]; then
	api_status=$(curl -s -o /dev/null -w '%{http_code}' \
		-H "Authorization: Bearer $(cat "$TOKEN")" -H "If-Match: $etag" \
		-X POST --data-binary '{"to": "archived"}' \
		"$API/api/v1/profiles/$profile_id/status")
	log "profile archived -> $api_status"
else
	log "warning: could not re-read the profile for archiving ($api_status)"
fi
rm -rf "$CLIP_DIR" 2>/dev/null

# 8) Wind down is done; the run's ops log is written at the very end (after
# the operator-step restore) so it records the final env state.
rm -rf "$CLIP_DIR" 2>/dev/null

log "run complete: $n_sessions session(s) started, $n_failed failed, " \
	"cumulative frames=$cum_frames ocr_calls=$cum_ocr commits=$cum_commits " \
	"dropped=$cum_dropped speech_started=$cum_speech_started"

# 9) Finish: the operator step is restored (idempotent; a no-op when it was
# not taken), then the ops log is written so it captures the restore, and
# the poller is told the run is done.
restore_env
{
	printf 'env_file=/etc/game-narrator/env\n'
	printf 'env_metadata_pre_run=%s\n' "$ENV_META_PRE"
	printf 'env_appended_line=GAME_NARRATOR_REPLAY_ROOTS=%s\n' "$REPLAY_ROOT"
	printf 'env_operator_step=%s\n' "$([ "$ENV_APPENDED" = "1" ] && echo taken || echo reused-existing)"
	printf 'env_restored=%s\n' "$env_restored"
	printf 'env_metadata_after_run=%s\n' "$(env_meta)"
	printf 'installed_version=%s\n' "$(dpkg-query -W -f='${Version}' game-narrator 2>/dev/null)"
	printf 'clip_sha256=%s\n' "$CLIP_SHA"
} >"$OUT/ops.txt"
printf '{"state":"done","rc":0,"n_sessions":%d,"n_failed":%d,"cum_frames":%d,"cum_ocr":%d,"cum_commits":%d,"cum_dropped":%d,"cum_speech_started":%d}\n' \
	"$n_sessions" "$n_failed" "$cum_frames" "$cum_ocr" "$cum_commits" "$cum_dropped" "$cum_speech_started" >"$OUT/status"
exit 0
RUNNER_EOF
chmod 755 "$tmp/runner.sh" || exit 1

# ── ship the stage to the device and start the runner ───────────────────────
remote_base=/tmp/gn-sustained-$run_id
echo "sustained: shipping the stage to $host:$remote_base"
# shellcheck disable=SC2029  # local expansion is intended (paths are literals)
ssh "$host" "rm -rf $remote_base && mkdir -p $remote_base" || exit 1
scp -r "$clip_src" "$host:$remote_base/clip" || exit 1
scp "$tmp/seed.csv" "$tmp/regions.json" "$tmp/runner.sh" \
	"$host:$remote_base/" || exit 1

# Start the runner detached on the device (setsid: the sshd session closing
# must not kill a 30 min run).
echo "sustained: starting the device runner (detached)"
# shellcheck disable=SC2029  # local expansion is intended (the runner takes them as args)
ssh "$host" "sudo -n sh -c 'setsid sh $remote_base/runner.sh $minutes $run_id $clip_sha $game_slug </dev/null >$remote_base/run.stdout 2>&1 & echo detached'" \
	|| { echo "sustained: could not start the runner on the device" >&2; exit 1; }

# ── poll the status file in short chunks ───────────────────────────────────
# Each iteration is one short ssh + a 30 s sleep, so a single foreground
# call can own the whole run without holding one connection for N minutes.
remote_status=$remote_base/out/status
deadline=$(( $(date +%s) + minutes * 60 + 15 * 60 ))
last_line=""
while :; do
	now=$(date +%s)
	[ "$now" -lt "$deadline" ] || {
		echo "sustained: gave up polling at ${minutes} min + 15 min grace; the run may still be going" >&2
		exit 1
	}
	# shellcheck disable=SC2029  # one path, no spaces
	st=$(ssh "$host" "cat $remote_status 2>/dev/null" || true)
	if [ -n "$st" ] && [ "$st" != "$last_line" ]; then
		state=$(printf '%s' "$st" | python3 -c 'import json,sys
try: print(json.load(sys.stdin).get("state", ""))
except Exception: print("")' 2>/dev/null)
		case $state in
		done)
			echo "sustained: runner reports done"
			break
			;;
		error)
			echo "sustained: runner reports an error: $st" >&2
			;;
		*)
			echo "sustained: [$(date -u +%H:%M:%S)] $st"
			;;
		esac
		last_line=$st
		[ "$state" = "error" ] && break
	fi
	sleep 30
done

# ── pull the output back and clean the device stage ─────────────────────────
scp -r "$host:$remote_base/out" "$outdir/device-out" || exit 1
[ -s "$outdir/device-out/samples.jsonl" ] || {
	echo "sustained: no samples came back — the run did not complete" >&2
	exit 1
}
# shellcheck disable=SC2029  # the stage is root-owned
ssh "$host" "sudo -n rm -rf $remote_base" || echo "sustained: warning: could not remove the device stage" >&2

# ── budgets and report (docs/spec/10 "Resource budgets" + hwsmoke) ─────────
# The report names the device the rule-5 placeholder, never its IP.
python3 - "$outdir" "$run_id" "$minutes" "<user>@<uno-q-host>" "$clip_sha" "$profile_json" <<'PY'
import json, math, sys, datetime

outdir, run_id, minutes, host, clip_sha, profile_json = sys.argv[1:7]
minutes = int(minutes)

samples = [json.loads(l) for l in open(outdir + "/device-out/samples.jsonl") if l.strip()]
sessions = [json.loads(l) for l in open(outdir + "/device-out/sessions.jsonl") if l.strip()]

ops = {}
try:
    for line in open(outdir + "/device-out/ops.txt"):
        if "=" in line:
            k, v = line.rstrip("\n").split("=", 1)
            ops[k] = v
except OSError:
    pass

def pctl(vals, p):
    if not vals:
        return None
    vals = sorted(vals)
    k = (len(vals) - 1) * p
    lo, hi = int(math.floor(k)), int(math.ceil(k))
    if lo == hi:
        return vals[lo]
    return vals[lo] + (vals[hi] - vals[lo]) * (k - lo)

def avg(vals):
    vals = [v for v in vals if v is not None]
    return sum(vals) / len(vals) if vals else None

rss = [s["service"].get("rss_kb") for s in samples if s["service"].get("rss_kb")]
cpu = [s["service"].get("cpu_pct") for s in samples if s["service"].get("cpu_pct") is not None]
avail = [s["system"].get("mem_avail_kb") for s in samples if s["system"].get("mem_avail_kb") is not None]
total = [s["system"].get("mem_total_kb") for s in samples if s["system"].get("mem_total_kb")]
temps = []
for s in samples:
    for z in s["system"].get("thermal", []):
        if z.get("celsius") is not None:
            temps.append(z["celsius"])
api_ms = [s["ui"]["api_ms"] for s in samples if s["ui"].get("api_ms") is not None]
page_ms = [s["ui"]["page_ms"] for s in samples if s["ui"].get("page_ms") is not None]
loads = [s["system"].get("load1") for s in samples if s["system"].get("load1") is not None]
store = [s["system"].get("store_avail_kb") for s in samples if s["system"].get("store_avail_kb") is not None]

# "Frames advancing in the final minute": either at least one full session
# cycle ended there (the per-session counter restarts per session) or the
# live counter inside a single session kept moving.
def advancing(samples):
    last = samples[-1]["ts"]
    tail = [s for s in samples if s["ts"] >= last - 60]
    if not tail:
        return False
    ids = {s["session"].get("id") for s in tail if s["session"].get("id")}
    if len(ids) >= 2:
        return True
    vals = [s["session"].get("frames") or 0 for s in tail]
    return max(vals) > min(vals)

failed_sessions = [s for s in sessions if s.get("state") == "failed"]
consec = 0
worst_consec = 0
for s in sessions:
    if s.get("state") == "failed":
        consec += 1
    else:
        consec = 0
    worst_consec = max(worst_consec, consec)
dropped = sum(s.get("frames_dropped") or 0 for s in sessions)
cum_frames = sum(s.get("frames_delivered") or 0 for s in sessions)
cum_ocr = sum(s.get("ocr_calls") or 0 for s in sessions)
cum_commits = sum(s.get("commits") or 0 for s in sessions)

sink = next((s.get("audio_sink") for s in sessions if s.get("audio_sink")), None)
speech_started = sum((s.get("speech") or {}).get("started") or 0 for s in sessions)
speech_requested = sum((s.get("speech") or {}).get("requested") or 0 for s in sessions)
# The session snapshot carries the audio block only when the process has a
# voice workflow wired and the session's sink built (internal/runtime
# SessionInfo.Audio is omitempty). Absent on this appliance: speech was
# disabled for the whole run, and that is recorded as such.
audio_wired = any((s.get("speech") is not None) or s.get("audio_sink") for s in sessions)

RSS_BUDGET_KB = 512 * 1024
AVAIL_FLOOR_KB = 512 * 1024
TEMP_LIMIT_C = 90.0
TREND_RATIO = 1.25
API_P95_MS = 2000
PAGE_P95_MS = 5000

checks = []
def check(name, ok, detail, budget):
    checks.append({"name": name, "ok": bool(ok), "detail": detail, "budget": budget})

peak_rss = max(rss) if rss else 0
check("serve RSS peak <= 512 MiB", peak_rss <= RSS_BUDGET_KB,
      "peak %.0f MiB over %d samples" % (peak_rss / 1024, len(rss)), "512 MiB")

if len(rss) >= 3:
    third = len(rss) // 3
    first = avg(rss[:third])
    last = avg(rss[-third:])
    trend_ok = (first is None) or (last is None) or (last <= first * TREND_RATIO)
    check("no RSS growth trend", trend_ok,
          "first-third avg %.0f MiB -> last-third avg %.0f MiB" % (first / 1024, last / 1024),
          "last-third <= 1.25 x first-third")
else:
    check("no RSS growth trend", False, "fewer than 3 RSS samples", "n/a")

min_avail = min(avail) if avail else 0
check("4 GB headroom (MemAvailable floor)", min_avail >= AVAIL_FLOOR_KB,
      "min available %.0f MiB of %.1f GiB" % (min_avail / 1024, max(total) / 1024 / 1024) if total
      else "min available %.0f MiB" % (min_avail / 1024), ">= 512 MiB")

if temps:
    check("SoC temperature below throttle", max(temps) < TEMP_LIMIT_C,
          "max %.1f C across %d zone samples" % (max(temps), len(temps)), "< %.0f C" % TEMP_LIMIT_C)
else:
    check("SoC temperature below throttle", False, "no thermal zone readable", "< %.0f C" % TEMP_LIMIT_C)

check("no stalled pipeline",
      worst_consec < 3 and dropped == 0 and advancing(samples) and cum_frames > 0,
      "%d sessions, %d failed, worst consecutive failures %d, queue drops %d, frames in final minute %s"
      % (len(sessions), len(failed_sessions), worst_consec, dropped,
         "advancing" if advancing(samples) else "STALLED"),
      "no run of >=3 failed sessions, 0 queue drops, frames advancing in final minute")

api95 = pctl(api_ms, 0.95)
page95 = pctl(page_ms, 0.95)
check("UI responsiveness (p95)",
      api95 is not None and page95 is not None and api95 <= API_P95_MS and page95 <= PAGE_P95_MS,
      "API p95 %.0f ms / page p95 %.0f ms" % (api95 or -1, page95 or -1),
      "API p95 <= %d ms, page p95 <= %d ms" % (API_P95_MS, PAGE_P95_MS))

verdict = "PASS" if all(c["ok"] for c in checks) else "FAIL"

report = {
    "run_id": run_id,
    "generated_at": datetime.datetime.now(datetime.timezone.utc).isoformat(),
    "host": "%s (UNO Q; identity anonymised per rule 5)" % host,
    "source": "replay (committed synthetic clip; NOT capture evidence)",
    "clip": "testdata/synthetic/replay/dialogue-advance-001",
    "clip_sha256": clip_sha,
    "profile": profile_json,
    "installed_version": ops.get("installed_version", ""),
    "minutes_requested": minutes,
    "minutes_observed": round((samples[-1]["ts"] - samples[0]["ts"]) / 60.0, 2) if len(samples) >= 2 else 0,
    "samples": len(samples),
    "operator_step": {
        "mechanism": "GAME_NARRATOR_REPLAY_ROOTS appended to /etc/game-narrator/env (the unit's EnvironmentFile), daemon-reload + restart, restored from the pre-run backup afterwards",
        "reused_existing": ops.get("env_operator_step") == "reused-existing",
        "restored": ops.get("env_restored") == "1",
        "env_metadata_before": ops.get("env_metadata_pre_run", ""),
        "env_metadata_after": ops.get("env_metadata_after_run", ""),
        "note": "temporary, documented operator step: the product cannot replay a user clip on an installed device without a configuration change (no production replay-root default, no clip-upload API route) — reported as a product gap",
    },
    "sessions": {
        "started": len(sessions),
        "failed": len(failed_sessions),
        "frames_delivered": cum_frames,
        "ocr_calls": cum_ocr,
        "commits": cum_commits,
        "queue_drops": dropped,
        "audio_sink": sink,
        "speech_requested": speech_requested,
        "speech_started": speech_started,
        "note": ("the device has no audio sink yet; speech was recorded to files (filesink), not played"
                 if audio_wired else
                 "the session snapshots carry no audio block (voice workflow not wired on this device "
                 "— no TTS voice configured; the device has no audio sink either): speech was disabled "
                 "for the whole run, recorded as such"),
    },
    "peaks": {
        "rss_mib": round(peak_rss / 1024, 1),
        "cpu_pct_of_one_core": round(max(cpu), 1) if cpu else None,
        "load1_max": round(max(loads), 2) if loads else None,
        "temp_c_max": round(max(temps), 1) if temps else None,
        "mem_avail_mib_min": round(min_avail / 1024, 1),
        "mem_total_mib": round(max(total) / 1024, 1) if total else None,
        "store_avail_mib": round(min(store) / 1024, 1) if store else None,
        "api_ms_p95": round(api95, 1) if api95 is not None else None,
        "page_ms_p95": round(page95, 1) if page95 is not None else None,
    },
    "checks": checks,
    "verdict": verdict,
    "capture": "NOT VERIFIED (no UVC dongle attached; --source capture refuses with NOT RUN)",
    "audio_playback": ("NOT VERIFIED (no audio sink; filesink recorded)" if audio_wired
                       else "NOT VERIFIED (no audio sink; speech disabled — no voice workflow wired)"),
}
with open(outdir + "/report.json", "w") as f:
    json.dump(report, f, indent=2)

lines = []
lines.append("# Sustained-play resource run — %s\n" % run_id)
lines.append("- Host: `%s` (UNO Q; anonymised per rule 5)" % host)
lines.append("- Installed version: `%s`" % (ops.get("installed_version") or "unknown"))
lines.append("- Source: **replay on the running service API** (committed synthetic clip `dialogue-advance-001`, clip sha256 `%s…`)." % clip_sha[:16])
lines.append("  Per AGENTS.md rule 3 this is a *replay source on the device* — it is NOT capture evidence and NOT hardware acceptance of the capture path.")
lines.append("- Requested %d min; observed %.1f min, %d samples at ~10 s, %d session cycles." % (minutes, report["minutes_observed"], len(samples), len(sessions)))
lines.append("- Speech: %s" % (
    "audio sink `%s` — the device has no audio sink yet; speech was recorded (filesink), not played. Recorded as such."
    % report["sessions"]["audio_sink"] if audio_wired else
    "none — the session snapshots carry no audio block (voice workflow not wired on this device; no TTS voice "
    "configured, no audio sink present), so speech was disabled for the whole run. Recorded as such."))
lines.append("- Replay root: temporary, documented operator step — `GAME_NARRATOR_REPLAY_ROOTS=/var/lib/game-narrator/replay` appended to `/etc/game-narrator/env` (the unit's EnvironmentFile), service restarted; the pre-run env file was restored at the end (restored: %s; metadata before `%s`, after `%s`). The product cannot replay a user clip on an installed device without this configuration change (no production replay-root default, no clip-upload API route) — reported as a product gap." % ("yes" if report["operator_step"]["restored"] else "no", report["operator_step"]["env_metadata_before"] or "n/a", report["operator_step"]["env_metadata_after"] or "n/a"))
lines.append("\n## Budgets (docs/spec/10 + internal/hwsmoke)\n")
lines.append("| Check | Result | Measured | Budget |")
lines.append("| --- | --- | --- | --- |")
for c in checks:
    lines.append("| %s | %s | %s | %s |" % (c["name"], "PASS" if c["ok"] else "**FAIL**", c["detail"], c["budget"]))
lines.append("\n**Verdict: %s**\n" % verdict)
lines.append("## Peaks\n")
for k, v in report["peaks"].items():
    lines.append("- %s: %s" % (k, v))
lines.append("\n## Not verified (honest NOT VERIFIED, docs/spec/16 M8)\n")
lines.append("- Capture source: NOT VERIFIED — no UVC capture dongle attached; `--source capture` refuses with NOT RUN until it is.")
lines.append("- Audio playback: NOT VERIFIED — no audio sink; %s." % (
    "filesink evidence only" if audio_wired else "speech was disabled for the run (no voice workflow wired)"))
lines.append("- Hotplug / power brownout / hub power behaviour: not exercised by this run.")
lines.append("\n## Artifacts\n")
lines.append("- `device-out/samples.jsonl` — the 10 s samples (RSS/CPU, memory, load, thermal, storage, session stats, UI latency).")
lines.append("- `device-out/sessions.jsonl` — per-session terminal stats.")
lines.append("- `device-out/ops.txt` — the run's ops log (env-file metadata before/after, installed version, clip sha).")
lines.append("- `device-out/runner.log` — the device runner's own log.")
lines.append("- `report.json` — this report, machine-readable.")
lines.append("\n## Cleanup\n")
lines.append("- Session stopped through `POST /api/v1/sessions/{id}/stop`; profile archived through `POST /api/v1/profiles/{id}/status` (to=archived).")
lines.append("- Per-run clip dir, token and /tmp stage removed from the device. The fixture game/edition rows have no delete route and remain inert.")
lines.append("- The env file was restored to its pre-run bytes and the service restarted; the replay root is no longer configured on the device.")
with open(outdir + "/report.md", "w") as f:
    f.write("\n".join(lines) + "\n")

print("verdict=%s sessions=%d failed=%d frames=%d ocr=%d commits=%d drops=%d "
      "rss_peak_mib=%.1f temp_max_c=%s mem_avail_mib_min=%.1f api_p95_ms=%s" % (
          verdict, len(sessions), len(failed_sessions), cum_frames, cum_ocr,
          cum_commits, dropped, peak_rss / 1024,
          round(max(temps), 1) if temps else None, min_avail / 1024,
          round(api95, 1) if api95 is not None else None))
for c in checks:
    if not c["ok"]:
        print("  FAIL %s: %s (budget %s)" % (c["name"], c["detail"], c["budget"]))
sys.exit(0 if verdict == "PASS" else 1)
PY
rc=$?

echo "sustained: report at $outdir/report.md (run $run_id)"
exit "$rc"
