#!/bin/sh
# Game Narrator .deb postinst (maintainer script, POSIX sh, idempotent).
# Invoked by dpkg with $1 = install | upgrade | replace | configure.
set -eu

# Dedicated system user (docs/spec/10 "UNO Q software deployment").
if ! getent passwd game-narrator >/dev/null 2>&1; then
    useradd --system \
        --user-group \
        --home-dir /var/lib/game-narrator \
        --shell /usr/sbin/nologin \
        --comment "Game Narrator service" \
        game-narrator
fi

# Durable data directory: created/owned for the service user. Mode 0700 per
# docs/spec/02 "Asset store contract" ("Directory 0700 for sensitive assets")
# and ADR 0014: it holds the SQLite catalogue, voice references and the
# speech cache, so group/other read access is a leak, not a convenience.
# The package NEVER deletes /var/lib/game-narrator (user data;
# docs/spec/15 "Debian/systemd package" — purge semantics in docs/release.md).
# On upgrade `install -d` re-applies owner/mode to the existing directory
# without touching its contents (verified: contents survive the chmod).
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

# Reload systemd so the unit shipped in /lib/systemd/system is picked up.
if command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl daemon-reload || true
fi

# Boot start: the appliance's product is this service (docs/spec/10
# "UNO Q software deployment": "Service managed by systemd" — it must come
# up at boot once the capture path is wired). Enable on FIRST install only:
# on upgrade/replace ($1 != install) a unit an admin disabled must stay
# disabled (Debian convention; ADR 0081). Not started here — the admin
# creates /etc/game-narrator/env first (manual 19, Step 5) and starts it.
if [ "$1" = "install" ] && command -v systemctl >/dev/null 2>&1 && [ -d /run/systemd/system ]; then
    systemctl enable game-narrator.service >/dev/null 2>&1 || true
fi

echo "game-narrator: install complete. The service is enabled at boot (first install only; an admin-disabled unit stays disabled across upgrades)."
echo "game-narrator: start it now with: sudo systemctl start game-narrator (or: sudo systemctl enable --now game-narrator if it is still disabled)"
echo "game-narrator: check devices and tools with: sudo game-narrator doctor --dev"
exit 0
