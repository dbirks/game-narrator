# syntax=docker/dockerfile:1
# Game Narrator runtime image (docs/spec/15 "OCI image design", ADR 0006,
# ADR 0012 Part 2, ADR 0131).
#
# The game-narrator binary is provided by the GoReleaser dockers_v2 build
# context at linux/${TARGETARCH}/game-narrator — this image never rebuilds
# Go code (ADR 0008: CGO_ENABLED=0, pure-Go SQLite).
#
# Reproducibility (ADR 0131): apt resolves the pinned package versions below
# from immutable snapshot.debian.org timestamps of the base image's build
# date, NOT from the live Debian archive (which only keeps the newest
# version of each package, so an upstream security update silently breaks
# exact pins). `docker build --target apt-pins` builds only that apt stage
# without the binary — scripts/release/verify-apt-pins.sh uses it as the
# fast CI guard. The final stage continues from `apt-pins`, so a full build
# and the stage share identical layers.
#
# Device access is NOT included by default: pass --device /dev/video*,
# --device /dev/snd* and --group-add 29:29 only when capture/audio are
# explicitly requested (docs/spec/10 "Deployment modes": "a container image
# does not automatically grant device access").
ARG BASE_IMAGE=debian@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
# snapshot.debian.org timestamps matching the base image's build date: the
# base's own /etc/apt/sources.list.d/debian.sources records the reference
# 20261005T000000Z, and these are the closest real snapshots (the security
# mirror is snapshotted on its own schedule). Verified to contain every
# pinned version below for BOTH linux/amd64 and linux/arm64 (ADR 0131).
ARG DEBIAN_SNAPSHOT_TS=20261005T023518Z
ARG DEBIAN_SECURITY_SNAPSHOT_TS=20261005T022253Z

# apt stage: frozen sources + exact package versions. Named so the guard
# (`docker build --target apt-pins`) can verify the pins in seconds without
# the GoReleaser binary in the build context.
FROM ${BASE_IMAGE} AS apt-pins

ARG DEBIAN_SNAPSHOT_TS
ARG DEBIAN_SECURITY_SNAPSHOT_TS

# Pinned distro packages (ADR 0012): V4L2 capture (ffmpeg), OCR with the eng
# language pack (tesseract-ocr), V4L2 probing (v4l-utils), TLS roots
# (ca-certificates), and wget for the HEALTHCHECK. The stock sources file is
# swapped for the frozen snapshot sources for the install, then restored, so
# the runtime image keeps the stock apt configuration (docs/spec/15: the
# container is not an apt appliance).
#
# Notes (ADR 0131):
#  - http:// (not https): the slim base has no CA store, and the first
#    package we install IS ca-certificates — the stock debian image fetches
#    deb.debian.org over plain http for the same reason. Integrity comes
#    from the GPG-signed InRelease/Release metadata (Signed-By + the image's
#    archive keyring), with Check-Valid-Until: no because a frozen InRelease
#    is by design past its short valid-until window.
#  - The pinned versions must exist in the frozen snapshots, for every
#    TARGETARCH; scripts/release/verify-apt-pins.sh fails early with a clear
#    message when they do not.
RUN mv /etc/apt/sources.list.d/debian.sources /etc/apt/sources.list.d/debian.sources.orig \
    && printf '%s\n' \
        'Types: deb' \
        "URIs: http://snapshot.debian.org/archive/debian/${DEBIAN_SNAPSHOT_TS}" \
        'Suites: bookworm bookworm-updates' \
        'Components: main' \
        'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg' \
        'Check-Valid-Until: no' \
        '' \
        'Types: deb' \
        "URIs: http://snapshot.debian.org/archive/debian-security/${DEBIAN_SECURITY_SNAPSHOT_TS}" \
        'Suites: bookworm-security' \
        'Components: main' \
        'Signed-By: /usr/share/keyrings/debian-archive-keyring.gpg' \
        'Check-Valid-Until: no' \
        > /etc/apt/sources.list.d/debian.sources \
    && apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates=20250419~deb12u1 \
        ffmpeg=7:5.1.9-0+deb12u1 \
        tesseract-ocr=5.3.0-2 \
        tesseract-ocr-eng=1:4.1.0-2 \
        v4l-utils=1.22.1-5+b2 \
        wget=1.21.3-1+deb12u1 \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/* \
    && rm /etc/apt/sources.list.d/debian.sources \
    && mv /etc/apt/sources.list.d/debian.sources.orig /etc/apt/sources.list.d/debian.sources

# Final stage: the pinned apt layer set plus the product binary.
FROM apt-pins

# Predefined by buildx per --platform; must be declared to be usable in COPY.
ARG TARGETARCH

COPY linux/${TARGETARCH}/game-narrator /usr/local/bin/game-narrator
RUN chmod 0755 /usr/local/bin/game-narrator

# Non-root service user (docs/spec/15); video/audio group membership so an
# operator can map the host devices in when explicitly requested.
RUN groupadd --system game-narrator \
    && useradd --system --gid game-narrator --home-dir /var/lib/game-narrator \
        --shell /usr/sbin/nologin --comment "Game Narrator service" game-narrator \
    && usermod -aG video,audio game-narrator

# Durable state (SQLite + content-addressed assets). 0700 per docs/spec/02
# "Asset store contract" and ADR 0014 (ADR 0063).
RUN install -d -o game-narrator -g game-narrator -m 0700 /var/lib/game-narrator
VOLUME /var/lib/game-narrator

# The container listens on all interfaces by design (the systemd unit path
# binds 127.0.0.1 by default); the operator must still authenticate and proxy
# any public exposure (docs/spec/10 "Security defaults for LAN deployment").
ENV GAME_NARRATOR_DATA_DIR=/var/lib/game-narrator \
    GAME_NARRATOR_LISTEN=0.0.0.0:8080
EXPOSE 8080

USER game-narrator
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD ["wget", "-qO-", "http://127.0.0.1:8080/api/v1/health/live"]

ENTRYPOINT ["game-narrator", "serve"]
