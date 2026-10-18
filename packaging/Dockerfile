# syntax=docker/dockerfile:1
# Game Narrator runtime image (docs/spec/15 "OCI image design", ADR 0006,
# ADR 0012 Part 2).
#
# The game-narrator binary is provided by the GoReleaser dockers_v2 build
# context at linux/${TARGETARCH}/game-narrator — this image never rebuilds
# Go code (ADR 0008: CGO_ENABLED=0, pure-Go SQLite).
#
# Device access is NOT included by default: pass --device /dev/video*,
# --device /dev/snd* and --group-add 29:29 only when capture/audio are
# explicitly requested (docs/spec/10 "Deployment modes": "a container image
# does not automatically grant device access").
ARG BASE_IMAGE=debian@sha256:7c7b2c966bc9ee8cedfeef67e0e279108992c77681fa595db4a9d65c06ccc587
FROM ${BASE_IMAGE}

# Predefined by buildx per --platform; must be declared to be usable in COPY.
ARG TARGETARCH

# Pinned distro packages (ADR 0012): V4L2 capture (ffmpeg), OCR with the eng
# language pack (tesseract-ocr), V4L2 probing (v4l-utils), TLS roots, and
# wget for the HEALTHCHECK. Versions verified against bookworm on 2026-07-20;
# re-pin base digest + versions on a rebuild cadence (docs/spec/15).
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates=20250419~deb12u1 \
        ffmpeg=7:5.1.9-0+deb12u1 \
        tesseract-ocr=5.3.0-2 \
        tesseract-ocr-eng=1:4.1.0-2 \
        v4l-utils=1.22.1-5+b2 \
        wget=1.21.3-1+deb12u1 \
    && apt-get clean \
    && rm -rf /var/lib/apt/lists/*

# Non-root service user (docs/spec/15); video/audio group membership so an
# operator can map the host devices in when explicitly requested.
RUN groupadd --system game-narrator \
    && useradd --system --gid game-narrator --home-dir /var/lib/game-narrator \
        --shell /usr/sbin/nologin --comment "Game Narrator service" game-narrator \
    && usermod -aG video,audio game-narrator

COPY linux/${TARGETARCH}/game-narrator /usr/local/bin/game-narrator
RUN chmod 0755 /usr/local/bin/game-narrator

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
