#!/bin/sh
# Game Narrator .deb prerm (maintainer script, POSIX sh, idempotent).
# Invoked by dpkg with $1 = remove(1) | upgrade(2) | replace(3) | obsoleted(4).
set -eu

# Stop the service before removal/upgrade (graceful drain: provider calls,
# speech, SQLite flush, camera/DAC close — docs/spec/10). Never touches user
# data in /var/lib/game-narrator.
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl stop game-narrator.service >/dev/null 2>&1 || true
fi
exit 0
