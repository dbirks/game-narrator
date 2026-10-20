#!/bin/sh
# env-setup.sh USER@HOST [--apply]
#
# Checks (and scaffolds) the service env file /etc/game-narrator/env on the
# UNO Q appliance. Read-only by default: reports whether the file exists,
# its mode/owner (want 600 root), and the NAMES of the GAME_NARRATOR_*
# variables it sets — never their values (AGENTS.md rule 5; the file is
# never printed). It also names the core variables that are still missing
# (gateway URL/token, the decision role and SPEECH_ASR at minimum).
#
# With --apply: if — and only if — the env file is missing, it scaffolds it
# from the packaged /etc/game-narrator/env.example at 0600 root:root
# (idempotent; an existing file is never touched). Because /etc/game-narrator
# is 750 root:game-narrator, the probe goes through passwordless sudo; if
# sudo is unavailable the file state is UNKNOWN and --apply refuses to
# scaffold (it could overwrite a file it cannot see). You then fill the
# values yourself (manual 19 step 5, references/install.md) — secrets stay
# out of band, and this script never takes or prints them.
#
# Usage: skills/uno-q-setup/scripts/env-setup.sh <user>@<uno-q-host> [--apply]
# Exit:  0 file present at 600 root (or scaffolded); 1 missing/failed.

set -u

apply=0
target=""

for a in "$@"; do
	case $a in
	--apply) apply=1 ;;
	-*)
		echo "unknown option: $a (usage: $0 USER@HOST [--apply])" >&2
		exit 2
		;;
	*)
		if [ -z "$target" ]; then
			target=$a
		else
			echo "unexpected argument: $a" >&2
			exit 2
		fi
		;;
	esac
done

if [ -z "$target" ]; then
	echo "usage: $0 USER@HOST [--apply]" >&2
	exit 2
fi

out=$(
	ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new \
		"$target" sh -s <<'REMOTE'
# /etc/game-narrator is 750 root:game-narrator — the ssh user cannot see
# the env file, so probe through passwordless sudo (names only, never
# values). Without sudo the file is not "missing", only unreadable.
if sudo -n true 2>/dev/null; then
	if sudo -n test -e /etc/game-narrator/env; then
		echo "env: present"
		echo "env-meta: $(sudo -n stat -c '%a %U' /etc/game-narrator/env 2>/dev/null)"
		echo "env-lines: $(sudo -n sh -c 'wc -l < /etc/game-narrator/env' 2>/dev/null | tr -d ' ')"
		echo "env-vars: $(sudo -n sh -c "grep -o '^[A-Za-z_][A-Za-z_0-9]*=' /etc/game-narrator/env" 2>/dev/null | sed 's/=$//' | sort -u | tr '\n' ' ')"
	else
		echo "env: missing"
	fi
	if sudo -n test -e /etc/game-narrator/env.example; then
		echo "example: present"
	else
		echo "example: missing"
	fi
else
	echo "env: unreadable"
	echo "example: unreadable"
fi
REMOTE
)
printf '%s\n' "$out"

field() {
	printf '%s\n' "$out" | sed -n "s/^${1}:[[:space:]]*//p" | head -n 1
}

if [ "$(field env)" = unreadable ]; then
	if [ "$apply" -eq 1 ]; then
		echo "FAIL: /etc/game-narrator is unreadable without sudo — cannot verify" >&2
		echo "      whether the env file exists; refusing to scaffold (it would" >&2
		echo "      risk overwriting a file we cannot see). Grant passwordless" >&2
		echo "      sudo (manual 19 Preconditions) and rerun with --apply." >&2
		exit 1
	fi
	echo
	echo "read-only: /etc/game-narrator is unreadable without sudo — the env"
	echo "file state is UNKNOWN, not 'missing'. Grant passwordless sudo"
	echo "(manual 19 Preconditions) and rerun."
	exit 1
fi

if [ "$(field env)" != present ]; then
	if [ "$apply" -eq 1 ]; then
		if [ "$(field example)" != present ]; then
			echo "FAIL: /etc/game-narrator/env.example not found — install the package first (install.sh --apply)" >&2
			exit 1
		fi
		echo "-- applying: scaffolding /etc/game-narrator/env from env.example (0600 root) --"
		if ! ssh -o BatchMode=yes -o ConnectTimeout=10 -o StrictHostKeyChecking=accept-new "$target" \
			'sudo install -m 0600 -o root -g root /etc/game-narrator/env.example /etc/game-narrator/env'; then
			echo "FAIL: scaffold failed" >&2
			exit 1
		fi
		echo "scaffolded. Fill the values now (manual 19 step 5):"
		echo "  GAME_NARRATOR_GATEWAY_URL / _GATEWAY_TOKEN, the role _MODEL lines"
		echo "  (incl. GAME_NARRATOR_ROLE_DECISION_MODEL — protocol systemone is the"
		echo "  default — and GAME_NARRATOR_ROLE_SPEECH_ASR_MODEL), plus the optional"
		echo "  import/listen/replay/capture-device lines and a comment naming the"
		echo "  audio sink. Then: sudo systemctl start game-narrator + doctor."
		exit 0
	fi
	echo
	echo "read-only: /etc/game-narrator/env is missing. Rerun with --apply to scaffold"
	echo "it from the packaged env.example (0600 root), then fill the values"
	echo "(manual 19 step 5; the file is never printed)."
	exit 1
fi

# present: report mode/owner and which core variables are still unset
meta=$(field env-meta)
vars=" $(field env-vars) "
missing=""
for v in \
	GAME_NARRATOR_GATEWAY_URL \
	GAME_NARRATOR_GATEWAY_TOKEN \
	GAME_NARRATOR_ROLE_DECISION_MODEL \
	GAME_NARRATOR_ROLE_SPEECH_ASR_MODEL
do
	case "$vars" in
	*" $v "*) : ;;
	*) missing="$missing $v" ;;
	esac
done

if [ "$meta" = "600 root" ]; then
	echo "env file: OK (0600 root, $(field env-lines) lines)"
else
	echo "env file: WARN mode/owner '$meta' (want 600 root) — sudo chmod 0600 /etc/game-narrator/env && sudo chown root:root /etc/game-narrator/env"
fi
if [ -n "$missing" ]; then
	echo "core variables still unset (names only):$missing"
	echo "      fill them per manual 19 step 5, then restart + doctor."
	exit 1
fi
echo "core variables: all set (values never printed)."
