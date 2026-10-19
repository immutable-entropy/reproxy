#!/usr/bin/env bash
# Builds an installable reProxy package.
#
#   packaging/package.sh <deb|rpm|win|mac> [--arch ARCH] [--output DIR]
#
# Each type is a self-contained packager under packaging/<type>/build.sh;
# this script only resolves the type, the common settings and the toolchain
# checks, then hands over. Run it through `make package TYPE=<type>`.

set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

usage() {
	cat >&2 <<EOF
Usage: packaging/package.sh <type> [options]

Types:
  deb    Debian/Ubuntu .deb           (implemented)
  rpm    Fedora/RHEL/openSUSE .rpm    (implemented)
  mac    macOS .app in a .dmg         (implemented; must run on macOS)
  win    Windows NSIS installer       (implemented; needs makensis)

Options:
  --arch ARCH     target architecture (default: this machine's)
  --output DIR    where to write the package (default: dist/)
  --version V     override the version (default: git describe)
  -h, --help      show this help

Environment:
  VERSION               same as --version
  REPROXY_MAINTAINER    "Name <email>" stamped into package metadata
  RELEASE               rpm only: the Release field (default 1)
  CODESIGN_IDENTITY     mac only: signing identity (default "-", ad-hoc)
  NOTARIZE_PROFILE      mac only: notarytool keychain profile
  MACDEPLOYQT, SKIP_DMG mac only: see packaging/mac/build.sh
  WIN_SIGN_PFX          win only: code-signing certificate (with its password
                        in WIN_SIGN_PASSWORD)
  WINDEPLOYQT, WINDRES  win only: see packaging/win/build.sh
EOF
}

TYPE="${1:-}"
case "$TYPE" in
	-h|--help|"") usage; [ -n "$TYPE" ] && exit 0 || exit 2 ;;
esac
shift

ARCH=""
while [ $# -gt 0 ]; do
	case "$1" in
		--arch)     ARCH="${2:-}"; shift 2 ;;
		--output)   DIST_DIR="${2:-}"; shift 2 ;;
		--version)  VERSION="${2:-}"; shift 2 ;;
		-h|--help)  usage; exit 0 ;;
		*)          die "unknown option '$1' (try --help)" ;;
	esac
done
export VERSION="${VERSION:-}"
export ARCH DIST_DIR

case "$TYPE" in
	deb|rpm|mac|win)
		exec "$REPO_ROOT/packaging/$TYPE/build.sh"
		;;
	*)
		die "unknown package type '$TYPE' (expected deb, rpm, win or mac)"
		;;
esac
