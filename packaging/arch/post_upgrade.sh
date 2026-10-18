#!/bin/sh
# Game Narrator Arch post_upgrade hook (pacman .install, POSIX sh, idempotent).
# nfpm wraps this file's content in `function post_upgrade() { ... }` inside
# the package's .INSTALL (nfpms archlinux.scripts.postupgrade). pacman runs it
# after the NEW files are extracted, on upgrades only (PKGBUILD(5), pacman 7.1:
# "during an upgrade operation, none of the install or remove functions will
# be called" — so post_install does NOT run on upgrades and this hook carries
# the same setup body). Two arguments are passed: new full version string, old
# full version string.
#
# Mirrors packaging/deb/postinst.sh on its upgrade path (the deb's prerm $1=2
# also stops the unit on upgrade: docs/spec/10, ADR 0053). Timing note: the
# stop happens here, after extraction — the running process keeps the old
# binary inode during extraction (safe on Linux), the graceful drain (provider
# calls, speech, SQLite flush, camera/DAC close) happens at the stop below,
# and the unit is left STOPPED across the upgrade: the operator starts it
# again to pick up the new binary (docs/release.md section 3 contract, same
# as the .deb).
set -eu

# Stop the service before the upgrade takes effect (graceful drain —
# docs/spec/10). Never touches user data in /var/lib/game-narrator.
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl stop game-narrator.service >/dev/null 2>&1 || true
fi

# Setup body: identical to post_install.sh (user via the shipped sysusers.d
# file with useradd fallback; data/config directories via install/chown —
# see post_install.sh for the systemd-tmpfiles rationale).
if ! getent passwd game-narrator >/dev/null 2>&1; then
    if command -v systemd-sysusers >/dev/null 2>&1; then
        systemd-sysusers >/dev/null 2>&1 || true
    fi
    if ! getent passwd game-narrator >/dev/null 2>&1; then
        useradd --system \
            --user-group \
            --home-dir /var/lib/game-narrator \
            --shell /usr/sbin/nologin \
            --comment "Game Narrator service" \
            game-narrator
    fi
fi
# 0700 per docs/spec/02 + ADR 0014; re-applied to the existing directory on
# upgrade without touching its contents (ADR 0063).
install -d -o game-narrator -g game-narrator -m 0700 /var/lib/game-narrator
install -d -o root -g game-narrator -m 0750 /etc/game-narrator
if [ -f /etc/game-narrator/config.yaml ]; then
    chown root:game-narrator /etc/game-narrator/config.yaml
    chmod 0640 /etc/game-narrator/config.yaml
fi
if [ ! -e /etc/game-narrator/env ]; then
    echo "game-narrator: create /etc/game-narrator/env from /etc/game-narrator/env.example (chmod 0600)"
fi
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi

echo "game-narrator: upgrade complete (service stopped by the package; start it with: systemctl start game-narrator)"
exit 0
