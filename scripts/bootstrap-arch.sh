#!/usr/bin/env bash
# bootstrap-arch.sh — install Game Narrator development dependencies on Arch Linux.
#
# Milestone 0 groundwork (docs/spec/16-implementation-and-acceptance.md,
# docs/spec/10-hardware-and-deployment.md "Arch development host").
#
# Properties:
#   * Idempotent: `pacman -S --needed` is a no-op for already-installed packages.
#   * Previewable: --dry-run prints every command and installs nothing.
#   * Non-invasive: does NOT enable or start services, does NOT change group
#     membership, does NOT touch any user configuration. See the "Manual steps"
#     note printed at the end.
set -euo pipefail

usage() {
  cat <<'USAGE'
Usage: scripts/bootstrap-arch.sh [--dry-run]

Install the Arch dev-host packages required by Game Narrator.
  --dry-run   Print the pacman commands without executing anything.
  -h, --help  Show this help.
USAGE
}

DRY_RUN=0
for arg in "$@"; do
  case "$arg" in
    --dry-run) DRY_RUN=1 ;;
    -h | --help)
      usage
      exit 0
      ;;
    *)
      echo "bootstrap-arch.sh: unknown argument: ${arg}" >&2
      usage >&2
      exit 2
      ;;
  esac
done

run() {
  printf '+'
  printf ' %q' "$@"
  printf '\n'
  if [ "$DRY_RUN" -eq 1 ]; then
    return 0
  fi
  "$@"
}

install_group() {
  local title="$1"
  shift
  echo
  echo "==> ${title}: ${*}"
  run sudo pacman -S --needed --noconfirm "$@"
}

echo "Arch bootstrap for Game Narrator (dry-run=${DRY_RUN})"

install_group "language and build toolchain" go nodejs pnpm make gcc
install_group "media, capture and OCR" ffmpeg v4l-utils tesseract tesseract-data-eng sqlite
install_group "release, signing and OCI" goreleaser syft cosign docker docker-buildx github-cli
install_group "virtual display and input automation" xorg-server-xvfb xorg-xwininfo xdotool
install_group "graphics drivers / tools" mesa mesa-utils vulkan-intel vulkan-tools
install_group "audio stack" pipewire pipewire-pulse pipewire-alsa wireplumber alsa-utils
install_group "utilities" jq go-yq imagemagick openssh
install_group "emulator and terminal data" dolphin-emu kitty-terminfo
install_group "browser runtime libraries and fonts" at-spi2-core libxcomposite libxdamage nss libxkbcommon libxrandr alsa-lib ttf-dejavu noto-fonts ttf-liberation

if [ "$DRY_RUN" -eq 1 ]; then
  echo
  echo "==> dry-run: nothing installed"
fi

cat <<'NOTES'

==> Manual steps (deliberately NOT performed by this script)
    sudo systemctl enable --now docker.service
    sudo usermod -aG docker "$USER"   # then log out and back in
    systemctl --user enable --now pipewire.socket pipewire-pulse.socket \
        wireplumber.service   # audio sink for playback tests
    Note: frontend tools (TypeScript, Vite, React, oxlint, oxfmt, Playwright)
    come from `pnpm install` in web/, not from pacman.
NOTES
