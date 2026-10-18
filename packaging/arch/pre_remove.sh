#!/bin/sh
# Game Narrator Arch pre_remove hook (pacman .install, POSIX sh, idempotent).
# nfpm wraps this file's content in `function pre_remove() { ... }` inside the
# package's .INSTALL (nfpms overrides.archlinux.scripts.preremove). pacman runs
# it right before the package files are removed; one argument is passed: the
# old full version string.
#
# Mirrors packaging/deb/prerm.sh on remove (docs/spec/10, ADR 0053).
# Never touches user data in /var/lib/game-narrator.
set -eu

# Stop the service before removal (graceful drain: provider calls, speech,
# SQLite flush, camera/DAC close — docs/spec/10).
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl stop game-narrator.service >/dev/null 2>&1 || true
fi
exit 0
