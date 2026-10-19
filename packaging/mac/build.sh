#!/usr/bin/env bash
# Builds a macOS .app bundle and the .dmg that carries it. Driven by
# packaging/package.sh; run it as
#
#   make package TYPE=mac              # arm64 on an Apple silicon Mac
#   make package TYPE=mac ARCH=x86_64  # Intel
#
# Bundle laid out:
#
#   reProxy.app/Contents/MacOS/reproxy                the binary
#   reProxy.app/Contents/Resources/reproxy.icns       the icon, 16..1024
#   reProxy.app/Contents/Resources/reproxy-mcp-service  LaunchAgent installer
#   reProxy.app/Contents/Resources/man/man1/reproxy.1   man reproxy
#   reProxy.app/Contents/Frameworks, PlugIns          Qt, copied by macdeployqt
#   reProxy.app/Contents/{Info.plist,PkgInfo}
#
# Unlike the .deb and .rpm, this packager must run ON macOS: internal/ui is
# cgo, so the binary needs the macOS SDK and a Qt 6 for the target
# architecture, and the bundle is assembled with Apple's own tools.
#
# Environment:
#   CODESIGN_IDENTITY   signing identity; "-" (ad-hoc, the default) or a
#                       "Developer ID Application: ..." name for a bundle
#                       other machines will open without a warning
#   NOTARIZE_PROFILE    notarytool keychain profile; set it to notarise and
#                       staple the .dmg (needs a Developer ID signature)
#   MACDEPLOYQT         path to macdeployqt, if it isn't on PATH or in the
#                       Homebrew/Qt prefixes this looks in
#   SKIP_DMG=1          stop after the .app, leaving it in build/mac/

set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/common.sh"

HERE="$REPO_ROOT/packaging/mac"

# --- host -------------------------------------------------------------------

[ "$(uname -s)" = "Darwin" ] || die "the mac packager must run on macOS.
       internal/ui is cgo against Qt 6, so the binary needs the macOS SDK,
       and the bundle is assembled with Apple's tools (iconutil, codesign,
       hdiutil). Run 'make package TYPE=mac' on the Mac you are building for."

# All of these ship with the Xcode command line tools, so one missing means
# the whole toolchain is: say so once rather than nine times.
for tool in hdiutil iconutil sips codesign plutil ditto otool; do
	command -v "$tool" >/dev/null 2>&1 ||
		die "'$tool' is missing; install the Xcode command line tools:
       xcode-select --install"
done

# --- target -----------------------------------------------------------------

# macOS architecture names are Go's apart from amd64/x86_64.
case "${ARCH:-}" in
	"")              MAC_ARCH="$(uname -m)" ;;
	arm64|aarch64)   MAC_ARCH=arm64 ;;
	x86_64|amd64)    MAC_ARCH=x86_64 ;;
	*) die "unsupported --arch '$ARCH' for mac (expected arm64 or x86_64)" ;;
esac

case "$MAC_ARCH" in
	arm64)  GOARCH=arm64 ;;
	x86_64) GOARCH=amd64 ;;
	*) die "unsupported machine architecture '$MAC_ARCH'" ;;
esac

if [ "$GOARCH" != "$(go env GOARCH)" ]; then
	note "cross-building for $MAC_ARCH: needs CGO_ENABLED=1, -arch $MAC_ARCH in"
	note "CGO_CFLAGS/CGO_LDFLAGS, and a Qt 6 built for $MAC_ARCH on PKG_CONFIG_PATH"
fi

VER="$(upstream_version)"

# CFBundleShortVersionString has to be a dotted number: Finder, the About
# box and notarisation all read it that way, and "0.0.0+git648f683" is not
# one. Keep the numeric part for macOS and carry the full git description in
# REProxyVersion, where it stays visible without breaking anything.
read -r V_MAJOR V_MINOR V_PATCH <<<"$(numeric_version)"
SHORT_VER="$V_MAJOR.$V_MINOR.$V_PATCH"

APP_DIR="$BUILD_DIR/mac/$APP_NAME.app"
CONTENTS="$APP_DIR/Contents"

say "Packaging $APP_NAME $VER ($MAC_ARCH)"

# --- bundle -----------------------------------------------------------------

rm -rf "$BUILD_DIR/mac"
mkdir -p "$CONTENTS/MacOS" "$CONTENTS/Resources/man/man1"

BIN="$CONTENTS/MacOS/$APP_ID"
build_binary darwin "$GOARCH" "$BIN"
chmod 0755 "$BIN"

say "Staging bundle"

# render_1024 OUT -- draws the one icon size assets/icons doesn't hold.
render_1024() {
	local out="$1" svg="$REPO_ROOT/assets/$APP_ID.svg"

	if command -v rsvg-convert >/dev/null 2>&1; then
		rsvg-convert -w 1024 -h 1024 "$svg" -o "$out" 2>/dev/null || true
		[ -s "$out" ] && return 0
	fi
	if command -v magick >/dev/null 2>&1; then
		magick -background none -density 768 "$svg" -resize 1024x1024 "$out" 2>/dev/null || true
		[ -s "$out" ] && return 0
	fi
	# No SVG renderer: upscale the 512. It loses detail, but this entry is
	# only ever drawn at 1024 in Quick Look and Finder's largest icon view.
	sips -s format png -z 1024 1024 "$(icon_png 512)" --out "$out" >/dev/null 2>&1 || true
	[ -s "$out" ]
}

# make_icns OUT -- iconutil names each entry by its point size and scale, so
# the 2x entries reuse the PNG of twice the size. assets/icons stops at 512,
# which covers every entry but 512x512@2x.
make_icns() {
	local out="$1"
	local iconset="$BUILD_DIR/mac/$APP_ID.iconset"
	rm -rf "$iconset"; mkdir -p "$iconset"

	local pair
	for pair in 16:16x16 32:16x16@2x 32:32x32 64:32x32@2x \
	            128:128x128 256:128x128@2x 256:256x256 512:256x256@2x 512:512x512; do
		install -m 0644 "$(icon_png "${pair%%:*}")" "$iconset/icon_${pair#*:}.png"
	done

	# Each renderer is tried and its output checked, because a renderer
	# being installed is no promise that it can read an SVG -- ImageMagick
	# in particular delegates that to librsvg and fails without it. An
	# iconset missing its largest entry is still valid, so the last resort
	# is to leave it out.
	local big="$iconset/icon_512x512@2x.png"
	if ! render_1024 "$big"; then
		rm -f "$big"
		note "no 1024px icon: Finder's largest size will scale the 512 down"
	fi

	iconutil -c icns "$iconset" -o "$out"
	note "$(basename "$out") ($(du -h "$out" | cut -f1))"
}
make_icns "$CONTENTS/Resources/$APP_ID.icns"

# The man page rides along so `man` can be pointed at the bundle; there is
# nowhere a .dmg may write under /usr/share.
install -m 0644 "$LINUX_DIR/$APP_ID.1" "$CONTENTS/Resources/man/man1/$APP_ID.1"

# The LaunchAgent installer, with the service's identity substituted in.
sed \
	-e "s|@APP_ID@|$APP_ID|g" \
	-e "s|@APP_NAME@|$APP_NAME|g" \
	-e "s|@SERVICE_ID@|$SERVICE_ID|g" \
	-e "s|@SERVICE_LABEL@|$MAC_SERVICE_LABEL|g" \
	-e "s|@SERVICE_PROXY_PORT@|$SERVICE_PROXY_PORT|g" \
	-e "s|@MCP_PORT@|$MCP_PORT|g" \
	"$HERE/$SERVICE_ID-service.in" > "$CONTENTS/Resources/$SERVICE_ID-service"
chmod 0755 "$CONTENTS/Resources/$SERVICE_ID-service"

# Info.plist. Every value is plain text with no '|', so sed uses it as the
# delimiter throughout (the homepage and paths contain '/').
sed \
	-e "s|@APP_ID@|$APP_ID|g" \
	-e "s|@APP_NAME@|$APP_NAME|g" \
	-e "s|@BUNDLE_ID@|$APP_BUNDLE_ID|g" \
	-e "s|@SHORT_VERSION@|$SHORT_VER|g" \
	-e "s|@RAW_VERSION@|$(raw_version)|g" \
	-e "s|@MIN_MACOS@|$MAC_MIN_VERSION|g" \
	-e "s|@CATEGORY@|$MAC_CATEGORY|g" \
	-e "s|@COPYRIGHT@|$(printf '%s %s. %s licensed.' "$(date +%Y)" "$APP_VENDOR" "$APP_LICENSE")|g" \
	"$HERE/Info.plist.in" > "$CONTENTS/Info.plist"

if grep -q '@[A-Z_]*@' "$CONTENTS/Info.plist" "$CONTENTS/Resources/$SERVICE_ID-service"; then
	die "unsubstituted placeholder: $(grep -ho '@[A-Z_]*@' "$CONTENTS/Info.plist" \
		"$CONTENTS/Resources/$SERVICE_ID-service" | sort -u | tr '\n' ' ')"
fi
plutil -lint "$CONTENTS/Info.plist" >/dev/null || die "generated Info.plist is not valid"

# The classic four-plus-four type/creator file. Modern macOS reads
# CFBundlePackageType instead, but some tooling still looks for this.
printf 'APPL????' > "$CONTENTS/PkgInfo"

note "Info.plist, PkgInfo, icns, man page, $SERVICE_ID-service"

# --- Qt ---------------------------------------------------------------------

# macdeployqt copies the Qt frameworks and the cocoa platform plugin into the
# bundle and rewrites the binary's load commands to @rpath, which is what
# makes the .app runnable on a Mac that has no Qt installed.
find_macdeployqt() {
	if [ -n "${MACDEPLOYQT:-}" ]; then printf '%s' "$MACDEPLOYQT"; return; fi
	if command -v macdeployqt >/dev/null 2>&1; then command -v macdeployqt; return; fi

	local prefix
	for prefix in "${QT_DIR:-}" \
	              "$(brew --prefix qt 2>/dev/null || true)" \
	              "$(brew --prefix qt@6 2>/dev/null || true)"; do
		if [ -n "$prefix" ] && [ -x "$prefix/bin/macdeployqt" ]; then
			printf '%s' "$prefix/bin/macdeployqt"
			return
		fi
	done
	printf ''
}

MDQ="$(find_macdeployqt)"
if [ -n "$MDQ" ]; then
	say "Bundling Qt with $(basename "$MDQ")"
	LOG="$BUILD_DIR/mac/macdeployqt.log"
	"$MDQ" "$APP_DIR" -always-overwrite > "$LOG" 2>&1 || {
		tail -20 "$LOG" >&2
		die "macdeployqt failed; full log in ${LOG#"$REPO_ROOT"/}"
	}
	note "$(find "$CONTENTS/Frameworks" -maxdepth 1 -name '*.framework' 2>/dev/null | wc -l | tr -d ' ') frameworks, $(du -sh "$APP_DIR" | cut -f1) bundle"
else
	note "macdeployqt not found: the bundle will only run where Qt 6 is installed."
	note "Install Qt (brew install qt) or set MACDEPLOYQT to bundle it."
fi

# Whatever happened above, say plainly whether anything outside the bundle is
# still being linked -- that is the difference between a .dmg anyone can open
# and one that only works on this machine.
EXTERNAL="$(otool -L "$BIN" | awk 'NR>1 {print $1}' | grep -Ev '^(@|/usr/lib/|/System/)' || true)"
if [ -n "$EXTERNAL" ]; then
	note "links libraries from outside the bundle; it will not run elsewhere:"
	printf '%s\n' "$EXTERNAL" | sed 's/^/        /'
fi

# --- signing ----------------------------------------------------------------

# Everything nested is signed before the bundle that contains it, because a
# bundle's signature covers its contents. Qt's frameworks arrive signed by
# whoever built them and macdeployqt's rewriting invalidates that, so this
# runs whether or not macdeployqt did.
#
# arm64 code must carry at least an ad-hoc signature or the kernel refuses to
# run it, so "-" is the floor, not an optional extra.
IDENTITY="${CODESIGN_IDENTITY:--}"
sign() {
	local target="$1"
	local args=(--force --sign "$IDENTITY")
	# The hardened runtime and a secure timestamp are what notarisation
	# requires; a secure timestamp is refused outright for an ad-hoc
	# signature, so neither is set for one. The runtime is a property of
	# running code, so a disk image takes the timestamp alone.
	if [ "$IDENTITY" != "-" ]; then
		args+=(--timestamp)
		case "$target" in
			*.dmg) ;;
			*) args+=(--options runtime) ;;
		esac
	fi
	# A framework is signed at the version inside it, not at the wrapper:
	# codesign treats Versions/A as the code and the rest as symlinks.
	if [ -d "$target/Versions/A" ]; then
		target="$target/Versions/A"
	fi
	codesign "${args[@]}" "$target" >/dev/null 2>&1 ||
		die "codesign failed for $target"
}

say "Signing ($([ "$IDENTITY" = "-" ] && echo "ad-hoc" || echo "$IDENTITY"))"
while IFS= read -r item; do
	sign "$item"
done < <(find "$CONTENTS/Frameworks" -maxdepth 1 \
	\( -name '*.framework' -o -name '*.dylib' \) 2>/dev/null)
while IFS= read -r item; do
	sign "$item"
done < <(find "$CONTENTS/PlugIns" -type f -name '*.dylib' 2>/dev/null)
sign "$APP_DIR"

codesign --verify --strict "$APP_DIR" >/dev/null 2>&1 ||
	die "the signed bundle does not verify"
if [ "$IDENTITY" = "-" ]; then
	note "ad-hoc: Gatekeeper will quarantine it on another Mac (see the note below)"
fi

if [ "${SKIP_DMG:-}" = "1" ]; then
	say "Done: ${APP_DIR#"$REPO_ROOT"/} (SKIP_DMG set, no .dmg built)"
	exit 0
fi

# --- dmg --------------------------------------------------------------------

# A disk image with the app and a symlink beside it is the whole install
# ritual on macOS: open it, drag one onto the other. There are no scripts to
# run, which is why the LaunchAgent is installed by hand afterwards.
STAGE="$BUILD_DIR/mac/dmg"
rm -rf "$STAGE"; mkdir -p "$STAGE"
ditto "$APP_DIR" "$STAGE/$APP_NAME.app"
ln -s /Applications "$STAGE/Applications"

mkdir -p "$DIST_DIR"
DMG="$DIST_DIR/$APP_NAME-$VER-$MAC_ARCH.dmg"
rm -f "$DMG"

say "Building $(basename "$DMG")"
# UDZO over HFS+ is the compressed, universally readable combination; APFS
# images need macOS 10.13 and buy nothing here.
hdiutil create -quiet \
	-volname "$APP_NAME $SHORT_VER" \
	-srcfolder "$STAGE" \
	-fs HFS+ -format UDZO -ov \
	"$DMG" || die "hdiutil failed"

# The image itself is signed too, so the download carries one signature from
# end to end.
sign "$DMG"

# --- notarisation -----------------------------------------------------------

if [ -n "${NOTARIZE_PROFILE:-}" ]; then
	[ "$IDENTITY" != "-" ] ||
		die "notarisation needs a Developer ID signature; set CODESIGN_IDENTITY"
	need xcrun
	say "Notarising (this waits on Apple)"
	xcrun notarytool submit "$DMG" --keychain-profile "$NOTARIZE_PROFILE" --wait ||
		die "notarytool rejected the image"
	xcrun stapler staple "$DMG" || die "stapling failed"
	note "stapled: it opens with no warning, offline"
fi

say "Done: ${DMG#"$REPO_ROOT"/} ($(du -h "$DMG" | cut -f1))"
note "install:  open \"$DMG\" and drag $APP_NAME to Applications"
note "command:  ln -s \"/Applications/$APP_NAME.app/Contents/MacOS/$APP_ID\" /usr/local/bin/$APP_ID"
note "service:  \"/Applications/$APP_NAME.app/Contents/Resources/$SERVICE_ID-service\" install"
if [ "$IDENTITY" = "-" ] && [ -z "${NOTARIZE_PROFILE:-}" ]; then
	note "unsigned by Apple's reckoning: on another Mac, open it from the Finder's"
	note "right-click menu once, or run: xattr -dr com.apple.quarantine /Applications/$APP_NAME.app"
fi
