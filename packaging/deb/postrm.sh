#!/bin/sh
# Game Narrator .deb postrm (maintainer script, POSIX sh, idempotent).
# Invoked by dpkg with $1 = install-failed(1) | upgrade-failed(2) |
# remove-or-purge(3) | replace(4).
#
# Purge semantics (docs/spec/15, docs/release.md): dpkg removes package files
# on "remove" and additionally config files on "purge". The user data
# directory /var/lib/game-narrator is NOT owned by the package and is NEVER
# deleted by these scripts — on purpose. Removing state is an explicit admin
# action documented in docs/release.md (backup/restore first).
set -eu

if [ "${1:-}" = "3" ]; then
    if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
        systemctl disable game-narrator.service >/dev/null 2>&1 || true
        systemctl daemon-reload >/dev/null 2>&1 || true
    fi
fi
exit 0
