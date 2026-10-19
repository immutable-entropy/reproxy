#!/usr/bin/env bash
# Runs once after the dev container is created.
set -euo pipefail
# The pre-commit hook runs the dependency soak check (make deps-check).
git config core.hooksPath .githooks
# The build-cache volume (see devcontainer.json) and its parent are created
# root-owned.
sudo chown vscode:vscode /home/vscode/.cache || true
sudo chown -R vscode:vscode /home/vscode/.cache/go-build || true

curl -fsSL https://claude.ai/install.sh | bash

# Qt 6 headers/libs for building the UI via miqt (cgo), plus the X11 and
# Wayland platform plugins so the window can render on the host desktop.
# fonts-inter is the UI font the theme prefers; DejaVu covers monospace.
# libnss3-tools provides certutil, used to install the proxy's HTTPS-
# interception CA into the launched browser's trust store.
sudo apt-get update
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    qt6-base-dev qt6-wayland libxcb-cursor0 fonts-inter fonts-dejavu-core libnss3-tools

# Headless UI testing: a virtual X display (Xvfb), screenshots (xwd from
# x11-apps, converted to PNG with imagemagick) and scripted mouse/keyboard
# input (xdotool). E.g.:
#   Xvfb :99 -screen 0 1440x900x24 & DISPLAY=:99 ./bin/reproxy &
#   DISPLAY=:99 xwd -root -silent | convert xwd:- shot.png
sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    xvfb x11-apps imagemagick xdotool

# Google Chrome, for Tools → Open Browser. A browser running inside the
# container sees the container's localhost, where the apps under test run.
# Ubuntu's own chromium/firefox packages are snap stubs that don't work in
# containers, so this uses Google's .deb; apt resolves its dependencies and
# the package registers Google's repository for updates. Google only
# publishes Chrome for Linux on amd64.
if [ "$(dpkg --print-architecture)" = "amd64" ]; then
    if ! command -v google-chrome >/dev/null; then
        chrome_deb="$(mktemp -d)/google-chrome-stable_current_amd64.deb"
        chmod 755 "$(dirname "$chrome_deb")" # readable by apt's sandboxed downloader
        curl -fsSL -o "$chrome_deb" https://dl.google.com/linux/direct/google-chrome-stable_current_amd64.deb
        sudo DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "$chrome_deb"
        rm -rf "$(dirname "$chrome_deb")"
    fi
else
    echo "note: Google Chrome isn't available for $(dpkg --print-architecture) Linux;" \
        "Tools → Open Browser needs a browser path in Settings → Browser." >&2
fi

# Pre-compile the Qt bindings. This takes several minutes the first time and
# is cached afterwards, so the first `make run` is quick.
cd "$(dirname "$0")/.."
go build ./... || echo "warning: initial build failed; run 'make build' to see why" >&2
