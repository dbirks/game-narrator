#!/bin/sh
# Game Narrator Arch post_remove hook (pacman .install, POSIX sh, idempotent).
# nfpm wraps this file's content in `function post_remove() { ... }` inside the
# package's .INSTALL (nfpms overrides.archlinux.scripts.postremove). pacman runs
# it right after the package files are removed; one argument is passed: the old
# full version string.
#
# Mirrors packaging/deb/postrm.sh on remove/purge (docs/spec/15, ADR 0053).
# Pacman has no separate "purge" verb — `pacman -R` removes every file the
# package owns, /etc/game-narrator included — so this hook runs its deb
# purge-equivalent work (disable + daemon-reload) on every removal.
#
# Purge semantics (docs/spec/15, docs/release.md): /var/lib/game-narrator is
# user data and is NEVER deleted by the package — on purpose. Removing state
# is an explicit admin action documented in docs/release.md (backup/restore
# first). The game-narrator system user is intentionally NOT removed here
# either (removing it mid-removal would orphan ownership of the not-yet-deleted
# data directory); `userdel game-narrator` after the data directory is gone is
# the admin's call.
set -eu

if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl disable game-narrator.service >/dev/null 2>&1 || true
    systemctl daemon-reload >/dev/null 2>&1 || true
fi
# The lingering user session (ADR 0091) goes with the package.
if command -v loginctl >/dev/null 2>&1; then
    loginctl disable-linger game-narrator >/dev/null 2>&1 || true
fi
exit 0
