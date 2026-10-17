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

# Durable data directory: created/owned for the service user.
# The package NEVER deletes /var/lib/game-narrator (user data;
# docs/spec/15 "Debian/systemd package" — purge semantics in docs/release.md).
install -d -o game-narrator -g game-narrator -m 0750 /var/lib/game-narrator

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

echo "game-narrator: install complete. Enable with: systemctl enable --now game-narrator"
echo "game-narrator: check devices and tools with: sudo game-narrator doctor --dev"
exit 0
