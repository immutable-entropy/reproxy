#!/usr/bin/env bash
# Builds a .deb for Debian/Ubuntu. Driven by packaging/package.sh; run it as
#
#   make package TYPE=deb
#
# Layout installed:
#
#   /usr/bin/reproxy                                  the binary
#   /usr/share/applications/reproxy.desktop           menu entry (searchable, pinnable)
#   /usr/share/icons/hicolor/<size>/apps/reproxy.png  icons, 16..512
#   /usr/share/icons/hicolor/scalable/apps/reproxy.svg
#   /usr/lib/systemd/user/reproxy-mcp.service         MCP server, per-user, off by default
#   /usr/share/man/man1/reproxy.1.gz
#   /usr/share/doc/reproxy/{copyright,changelog.Debian.gz}

set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/common.sh"

HERE="$REPO_ROOT/packaging/deb"
need dpkg-deb gzip

# --- target -----------------------------------------------------------------

# Debian architecture names differ from Go's; map the ones we can build.
case "${ARCH:-}" in
	"")                  DEB_ARCH="$(dpkg --print-architecture 2>/dev/null || echo amd64)" ;;
	amd64|x86_64)        DEB_ARCH=amd64 ;;
	arm64|aarch64)       DEB_ARCH=arm64 ;;
	armhf|arm)           DEB_ARCH=armhf ;;
	i386|386)            DEB_ARCH=i386 ;;
	*) die "unsupported --arch '$ARCH' for deb (expected amd64, arm64, armhf or i386)" ;;
esac

case "$DEB_ARCH" in
	amd64) GOARCH=amd64 ;;
	arm64) GOARCH=arm64 ;;
	armhf) GOARCH=arm ;;
	i386)  GOARCH=386 ;;
esac

if [ "$GOARCH" != "$(go env GOARCH)" ]; then
	note "cross-building for $DEB_ARCH; internal/ui needs a matching Qt 6 + cgo toolchain"
fi

VER="$(upstream_version)"
PKG="${APP_ID}_${VER}_${DEB_ARCH}"
STAGE="$BUILD_DIR/deb/$PKG"

say "Packaging $APP_NAME $VER ($DEB_ARCH)"

# --- stage ------------------------------------------------------------------

rm -rf "$STAGE"
mkdir -p "$STAGE/DEBIAN" "$STAGE/usr/bin" "$STAGE/usr/share/doc/$APP_ID"

BIN="$STAGE/usr/bin/$APP_ID"
build_binary linux "$GOARCH" "$BIN"
chmod 0755 "$BIN"

say "Staging files"
stage_linux "$STAGE"

install -m 0644 "$HERE/copyright" "$STAGE/usr/share/doc/$APP_ID/copyright"

# Debian policy wants man pages gzipped; stage_linux leaves them plain
# because rpmbuild compresses them itself.
gzip -9n "$STAGE/usr/share/man/man1/$APP_ID.1"
chmod 0644 "$STAGE/usr/share/man/man1/$APP_ID.1.gz"

# A changelog is required of every binary package. Without a real packaging
# history to draw on, record this build.
{
	printf '%s (%s) unstable; urgency=medium\n\n' "$APP_ID" "$VER"
	printf '  * Build of %s.\n\n' "$(raw_version)"
	printf ' -- %s  %s\n' "$APP_MAINTAINER" "$(date -R)"
} | gzip -9nc > "$STAGE/usr/share/doc/$APP_ID/changelog.Debian.gz"
chmod 0644 "$STAGE/usr/share/doc/$APP_ID/changelog.Debian.gz"

for script in postinst prerm postrm; do
	install -m 0755 "$HERE/$script" "$STAGE/DEBIAN/$script"
done

# The settings file and the CA under ~/.reproxy are written by the app at
# runtime, not shipped, so there are no conffiles.

# --- dependencies -----------------------------------------------------------

# The binary links Qt 6 (internal/ui is the only cgo package). Ask dpkg which
# packages provide the libraries it actually needs, rather than guessing and
# going stale. dpkg-shlibdeps insists on a debian/control next to its working
# directory, so give it a throwaway one.
resolve_depends() {
	local fallback="libc6, libqt6core6t64 | libqt6core6, libqt6gui6, libqt6widgets6, libqt6dbus6, libstdc++6"

	command -v dpkg-shlibdeps >/dev/null 2>&1 || {
		note "dpkg-shlibdeps not installed; using a fixed dependency list" >&2
		printf '%s' "$fallback"; return
	}

	local work="$BUILD_DIR/deb/shlibdeps"
	rm -rf "$work"; mkdir -p "$work/debian"
	printf 'Source: %s\n\nPackage: %s\nArchitecture: %s\n' \
		"$APP_ID" "$APP_ID" "$DEB_ARCH" > "$work/debian/control"

	local out
	if out="$(cd "$work" && dpkg-shlibdeps -O --ignore-missing-info \
			--warnings=0 -e"$BIN" 2>/dev/null)"; then
		# Output looks like: shlibs:Depends=libc6 (>= 2.34), libqt6core6 ...
		out="${out#*shlibs:Depends=}"
		out="${out%%$'\n'*}"
		if [ -n "$out" ]; then printf '%s' "$out"; return; fi
	fi
	note "dpkg-shlibdeps found nothing usable; using a fixed dependency list" >&2
	printf '%s' "$fallback"
}

say "Resolving library dependencies"
DEPENDS="$(resolve_depends)"
note "$DEPENDS"

INSTALLED_KB="$(du -ks "$STAGE" | cut -f1)"

cat > "$STAGE/DEBIAN/control" <<EOF
Package: $APP_ID
Version: $VER
Section: devel
Priority: optional
Architecture: $DEB_ARCH
Maintainer: $APP_MAINTAINER
Installed-Size: $INSTALLED_KB
Depends: $DEPENDS
Recommends: ca-certificates
Suggests: google-chrome-stable | chromium | firefox
Homepage: $APP_HOMEPAGE
Description: $APP_SUMMARY
 reProxy runs a local SOCKS5 proxy that records the HTTP traffic passing
 through it, and a desktop window for inspecting that traffic, holding
 requests and responses for editing, and replaying them.
 .
 It also serves its tools to MCP clients over loopback HTTP, so an agent can
 watch, intercept, edit and replay traffic alongside you. Requests an agent
 sends are confined to the domain the original request targeted.
 .
 The MCP server runs inside the app while a window is open. To keep it
 available without one, enable the per-user reproxy-mcp service.
EOF

# --- build ------------------------------------------------------------------

# --root-owner-group writes root:root ownership into the archive without
# needing fakeroot or actual root.
mkdir -p "$DIST_DIR"
DEB="$DIST_DIR/$PKG.deb"
say "Building $PKG.deb"
dpkg-deb --root-owner-group --build "$STAGE" "$DEB" >/dev/null

say "Done: ${DEB#"$REPO_ROOT"/} ($(du -h "$DEB" | cut -f1))"
note "install:  sudo apt install $DEB"
note "remove:   sudo apt remove $APP_ID"
note "purge:    sudo apt purge $APP_ID"
