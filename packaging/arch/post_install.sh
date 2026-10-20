#!/bin/sh
# Game Narrator Arch post_install hook (pacman .install, POSIX sh, idempotent).
# nfpm wraps this file's content in `function post_install() { ... }` inside the
# package's .INSTALL (packaging/arch/*.sh -> .goreleaser.yaml nfpms overrides).
# pacman runs it right after the package files are extracted, on a FRESH
# install only — on upgrades pacman runs post_upgrade instead (PKGBUILD(5),
# pacman 7.1: "during an upgrade operation, none of the install or remove
# functions will be called") — so post_upgrade.sh carries the same body.
# One argument is passed: the new full version string.
#
# Mirrors packaging/deb/postinst.sh behaviour (docs/spec/10, ADR 0053).
# User and directory definitions are the declarative sysusers.d/tmpfiles.d
# files this package ships: this hook applies them immediately (pacman's own
# systemd-sysusers post-transaction hook runs AFTER the scriptlets, so a
# fresh install must not rely on it), and the standard systemd-sysusers /
# systemd-tmpfiles boot services re-assert them at every boot. Directory
# ownership is set with install/chown (libc NSS) rather than systemd-tmpfiles:
# on the 2026-10-18 dev host systemd-tmpfiles could not resolve any non-root
# user name ("Unknown user", verified with root:root and david:david lines),
# while useradd/systemd-sysusers creation and libc NSS resolution work.
# /var/lib/game-narrator is user data and is NEVER deleted by the package
# (docs/spec/15, docs/release.md section 5).
set -eu

# Dedicated system user (docs/spec/10), from /usr/lib/sysusers.d/
# game-narrator.conf when systemd-sysusers is available (declarative),
# useradd fallback (deb parity) when it is not.
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

# Durable data directory: created/owned for the service user. Mode 0700 per
# docs/spec/02 "Asset store contract" ("Directory 0700 for sensitive assets")
# and ADR 0014 (catalogue, voice references, speech cache — no group/other
# read). `install -d` re-applies the mode on an existing directory without
# touching its contents (upgrade path, ADR 0063).
install -d -o game-narrator -g game-narrator -m 0700 /var/lib/game-narrator

# Configuration directory: root-owned, group-readable by the service user so
# the admin can edit /etc/game-narrator/config.yaml without touching state.
install -d -o root -g game-narrator -m 0750 /etc/game-narrator
if [ -f /etc/game-narrator/config.yaml ]; then
    chown root:game-narrator /etc/game-narrator/config.yaml
    chmod 0640 /etc/game-narrator/config.yaml
fi

# The runtime env file (tokens, device identities) is NOT shipped: the admin
# creates /etc/game-narrator/env from /etc/game-narrator/env.example at 0600.
if [ ! -e /etc/game-narrator/env ]; then
    echo "game-narrator: create /etc/game-narrator/env from /etc/game-narrator/env.example (chmod 0600)"
fi

# The lingering user session for Bluetooth audio (ADR 0091): the A2DP sink
# node (bluez_output.<MAC>.1) lives in the service user's USER PipeWire
# instance (WirePlumber's bluez5 module), which only exists while the user
# manager runs; on a headless appliance there is no login, so the user
# manager is made to run via linger. Idempotent; `|| true` without logind.
# The WirePlumber headless override is the file ADR 0084 verified on the
# real board — only-if-missing, so an admin-placed copy is never clobbered.
if command -v loginctl >/dev/null 2>&1; then
    loginctl enable-linger game-narrator >/dev/null 2>&1 || true
fi
install -d -o game-narrator -g game-narrator -m 0700 /var/lib/game-narrator/.config/wireplumber/wireplumber.conf.d
if [ ! -e /var/lib/game-narrator/.config/wireplumber/wireplumber.conf.d/80-bluez-headless.conf ]; then
    printf 'wireplumber.profiles = { main = { monitor.bluez.seat-monitoring = disabled } }\n' \
        > /var/lib/game-narrator/.config/wireplumber/wireplumber.conf.d/80-bluez-headless.conf
    chown game-narrator:game-narrator /var/lib/game-narrator/.config/wireplumber/wireplumber.conf.d/80-bluez-headless.conf
    chmod 0600 /var/lib/game-narrator/.config/wireplumber/wireplumber.conf.d/80-bluez-headless.conf
fi

# Reload systemd so the unit shipped in /usr/lib/systemd/system is picked up.
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi

echo "game-narrator: install complete. Start with: systemctl start game-narrator"
echo "game-narrator: check devices and tools with: sudo game-narrator doctor --dev"
exit 0
