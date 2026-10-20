#!/usr/bin/env bash
# Shared helpers for the packagers in packaging/<type>/. Sourced, not run.
#
# Everything here is platform-agnostic: naming, version normalisation,
# building the binary and rasterising the icon. Each packager under
# packaging/<type>/build.sh takes it from there.

set -euo pipefail

# --- identity ---------------------------------------------------------------

APP_ID="reproxy"            # binary name, package name, icon/desktop basename
APP_NAME="reProxy"          # human-readable name
APP_SUMMARY="Intercepting proxy for inspecting, editing and replaying HTTP traffic"
APP_VENDOR="reProxy"
APP_HOMEPAGE="https://github.com/immutable-entropy/reproxy"
APP_MAINTAINER="${REPROXY_MAINTAINER:-Kayleigh <0x95private@pm.me>}"

# PLACEHOLDER: the repository declares no licence -- there is no LICENSE file
# and neither README.md nor go.mod names one. Both packagers stamp this value
# into their metadata (deb/copyright and the rpm License: tag), so set it here
# once the project picks a licence, and update packaging/deb/copyright to
# match.
APP_LICENSE="MIT"

# The MCP server's systemd/launchd service name, and its default port
# (config.DefaultMCPPort).
SERVICE_ID="reproxy-mcp"
MCP_PORT=3917

# The proxy port the service runs on, deliberately not the app's default of
# 7193: whichever process starts first takes 7193 and later ones step up, so
# a service sitting there would swallow the traffic from a browser pointed at
# it while the user watched an empty window. Keep this in step with
# packaging/linux/reproxy-mcp.service, which hard-codes it in ExecStart.
SERVICE_PROXY_PORT=7293

# macOS identity. The bundle identifier is what launchd, the keychain and
# Gatekeeper know the app by, so it must not change once a version ships.
# 11.0 is the floor because it is the first macOS for Apple silicon.
APP_BUNDLE_ID="io.github.reproxy.reproxy"
MAC_SERVICE_LABEL="io.github.reproxy.mcp"
MAC_MIN_VERSION="11.0"
MAC_CATEGORY="public.app-category.developer-tools"

# Windows identity. WIN_REG_KEY is the name under
# HKLM\Software\Microsoft\Windows\CurrentVersion\Uninstall that Add/Remove
# Programs lists the app by, so like the bundle identifier it must not change
# once a version ships. WIN_TASK_NAME is the scheduled task the MCP service
# runs as -- the counterpart of the systemd unit and the LaunchAgent.
WIN_REG_KEY="reproxy"
WIN_TASK_NAME="reProxy MCP Server"

# --- paths ------------------------------------------------------------------

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
BUILD_DIR="$REPO_ROOT/build"       # staging; disposable, gitignored
# package.sh exports DIST_DIR when --output is given, and each packager
# sources this file again, so the default must not overwrite it.
DIST_DIR="${DIST_DIR:-$REPO_ROOT/dist}"   # finished packages land here

# Files installed identically by every Linux packager: the desktop entry, the
# systemd user unit and the man page. Only the metadata around them (control
# vs spec) differs, so they live here rather than in deb/ or rpm/.
LINUX_DIR="$REPO_ROOT/packaging/linux"

# --- logging ----------------------------------------------------------------

if [ -t 1 ] && [ -z "${NO_COLOR:-}" ]; then
	_c_dim=$'\033[2m'; _c_red=$'\033[31m'; _c_grn=$'\033[32m'; _c_off=$'\033[0m'
else
	_c_dim=""; _c_red=""; _c_grn=""; _c_off=""
fi

say()  { printf '%s==>%s %s\n' "$_c_grn" "$_c_off" "$*"; }
note() { printf '%s    %s%s\n' "$_c_dim" "$*" "$_c_off"; }
die()  { printf '%serror:%s %s\n' "$_c_red" "$_c_off" "$*" >&2; exit 1; }

need() {
	for t in "$@"; do
		command -v "$t" >/dev/null 2>&1 || die "'$t' is required but not installed"
	done
}

# --- version ----------------------------------------------------------------

# raw_version is what the Makefile uses: a git description, or "dev".
raw_version() {
	if [ -n "${VERSION:-}" ]; then
		printf '%s' "$VERSION"
		return
	fi
	git -C "$REPO_ROOT" describe --tags --always --dirty 2>/dev/null || printf 'dev'
}

# upstream_version turns a git description into something every packager
# accepts: it must start with a digit, and must not contain '-' (which
# separates the upstream version from the package revision in both deb and
# rpm). Examples:
#
#   v1.2.3             -> 1.2.3
#   v1.2.3-5-gabc1234  -> 1.2.3+5+gabc1234
#   v1.2.3-dirty       -> 1.2.3+dirty
#   648f683            -> 0.0.0+git648f683       (no tag reachable)
#   648f683-dirty      -> 0.0.0+git648f683+dirty
#   dev                -> 0.0.0+gitdev
#
# Note that "starts with a digit" is not enough to recognise a tag: when no
# tag is reachable, `git describe --always` prints a bare commit hash, and
# half of those start with a digit too. A release tag is identified by its
# leading numeric component instead.
upstream_version() {
	local v first
	v="$(raw_version)"
	v="${v#v}"              # strip a leading v from the tag
	first="${v%%-*}"        # the part before git's first '-' separator

	if [[ "$first" =~ ^[0-9]+(\.[0-9]+)*$ ]]; then
		: # 1, 1.2, 1.2.3 ...: a real tag, keep it
	else
		v="0.0.0+git${v}"   # a bare hash, or "dev"
	fi

	# '-' separates the upstream version from the package revision in both
	# deb and rpm; '+' is safe in both.
	v="${v//-/+}"
	# Collapse any run of '+' the substitutions above may have produced.
	while [[ "$v" == *"++"* ]]; do v="${v//++/+}"; done
	printf '%s' "$v"
}

# numeric_version prints the leading numeric components of the upstream
# version as "MAJOR MINOR PATCH", each defaulting to 0. Both desktop
# platforms insist on a dotted number where git is happy with
# "0.0.0+git648f683": macOS for CFBundleShortVersionString, Windows for the
# VERSIONINFO resource. The full description travels beside it in a field
# that takes free text.
numeric_version() {
	local v major minor patch
	v="$(upstream_version)"
	v="${v%%+*}"
	IFS=. read -r major minor patch _ <<<"$v"
	[[ "$major" =~ ^[0-9]+$ ]] || major=0
	[[ "$minor" =~ ^[0-9]+$ ]] || minor=0
	[[ "$patch" =~ ^[0-9]+$ ]] || patch=0
	printf '%s %s %s' "$major" "$minor" "$patch"
}

# --- build ------------------------------------------------------------------

# host_arch prints the package-native architecture name for the target
# platform, given a Go GOARCH.
go_arch() { go env GOARCH; }

# build_binary GOOS GOARCH OUTPUT — compiles cmd/reproxy with the release
# flags the Makefile uses, stamping the raw (human-readable) version.
#
# The flags deliberately match `make build` exactly.
# Adding anything that changes the build cache key to only one of them gives
# the Qt bindings a second cache entry, so switching between `make build` and
# `make package` recompiles them -- about 20 minutes each way.
#
# Cross-compiling is only possible for a target whose Qt toolchain is present:
# internal/ui uses cgo. Building for the host needs nothing extra.
build_binary() {
	local goos="$1" goarch="$2" out="$3"
	local ver; ver="$(raw_version)"
	local ldflags="-s -w -X main.version=$ver"

	# A Windows desktop application must be linked for the GUI subsystem or
	# Windows opens a console window behind its own window. cmd/reproxy
	# reattaches to the parent console when it was started from one, so the
	# command line still works (see cmd/reproxy/console_windows.go).
	if [ "$goos" = windows ]; then
		ldflags="$ldflags -H windowsgui"
	fi

	say "Building $APP_ID $ver for $goos/$goarch"
	mkdir -p "$(dirname "$out")"
	(
		cd "$REPO_ROOT"
		GOOS="$goos" GOARCH="$goarch" \
			go build -trimpath -ldflags "$ldflags" -o "$out" ./cmd/reproxy
	) || die "build failed"
	note "$(du -h "$out" | cut -f1) -> ${out#"$REPO_ROOT"/}"
}

# --- icons ------------------------------------------------------------------

# Sizes the freedesktop icon theme (and Windows/macOS packagers) want.
ICON_SIZES=(16 22 24 32 48 64 128 256 512)

# icon_png SIZE — prints the path to a rasterised icon of that size,
# generating assets/icons/ on demand from assets/reproxy.svg.
icon_png() {
	local size="$1"
	local png="$REPO_ROOT/assets/icons/${APP_ID}-${size}.png"
	[ -f "$png" ] || "$REPO_ROOT/packaging/mkicons.sh" >&2
	[ -f "$png" ] || die "no icon for ${size}x${size}; run packaging/mkicons.sh"
	printf '%s' "$png"
}

# icon_ico — prints the path to the multi-size Windows icon, generating
# assets/icons/ on demand like icon_png does.
icon_ico() {
	local ico="$REPO_ROOT/assets/icons/${APP_ID}.ico"
	[ -f "$ico" ] || "$REPO_ROOT/packaging/mkicons.sh" >&2
	[ -f "$ico" ] || die "no ${APP_ID}.ico; run packaging/mkicons.sh"
	printf '%s' "$ico"
}

# --- staging ----------------------------------------------------------------

# stage_linux ROOT — lays out the files every Linux package installs, under
# ROOT as if it were /. The binary is not built here: the caller does that
# first, so it can choose the target and see the build fail on its own.
#
# The man page is left uncompressed. Debian policy wants it gzipped, which
# packaging/deb/build.sh does; rpmbuild compresses it itself and would
# otherwise double up.
stage_linux() {
	local root="$1"

	install -d "$root/usr/share/applications" \
	           "$root/usr/share/icons/hicolor/scalable/apps" \
	           "$root/usr/lib/systemd/user" \
	           "$root/usr/share/man/man1"

	install -m 0644 "$LINUX_DIR/$APP_ID.desktop" \
		"$root/usr/share/applications/$APP_ID.desktop"
	# /usr/lib/systemd/user is the user-unit directory on both Debian and
	# Fedora (rpm's %_userunitdir), so this path needs no per-distro case.
	install -m 0644 "$LINUX_DIR/$SERVICE_ID.service" \
		"$root/usr/lib/systemd/user/$SERVICE_ID.service"
	install -m 0644 "$LINUX_DIR/$APP_ID.1" "$root/usr/share/man/man1/$APP_ID.1"
	install -m 0644 "$REPO_ROOT/assets/$APP_ID.svg" \
		"$root/usr/share/icons/hicolor/scalable/apps/$APP_ID.svg"

	local size dir
	for size in "${ICON_SIZES[@]}"; do
		dir="$root/usr/share/icons/hicolor/${size}x${size}/apps"
		install -d "$dir"
		install -m 0644 "$(icon_png "$size")" "$dir/$APP_ID.png"
	done
	note "${#ICON_SIZES[@]} icon sizes + scalable, desktop entry, service unit, man page"
}
