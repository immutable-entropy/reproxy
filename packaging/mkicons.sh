#!/usr/bin/env bash
# Rasterises assets/reproxy.svg into the PNG sizes the packagers install.
#
# The PNGs are committed, so packaging a release needs no SVG renderer. Run
# this only after changing assets/reproxy.svg, and commit what it writes.

set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/common.sh"

SVG="$REPO_ROOT/assets/reproxy.svg"
OUT="$REPO_ROOT/assets/icons"

[ -f "$SVG" ] || die "missing $SVG"

# rsvg-convert keeps the geometry exact; ImageMagick's SVG handling is a
# fallback because its renderer is less faithful.
if command -v rsvg-convert >/dev/null 2>&1; then
	render() { rsvg-convert -w "$1" -h "$1" "$SVG" -o "$2"; }
elif command -v convert >/dev/null 2>&1; then
	render() { convert -background none -density 384 "$SVG" -resize "${1}x${1}" "$2"; }
else
	die "need rsvg-convert (librsvg2-bin) or convert (imagemagick)"
fi

mkdir -p "$OUT"
say "Rendering icons from ${SVG#"$REPO_ROOT"/}"
for size in "${ICON_SIZES[@]}"; do
	render "$size" "$OUT/${APP_ID}-${size}.png"
	note "${size}x${size}"
done

# The Windows packager wants one file holding several sizes, and MSYS2 is an
# awkward place to ask for an icon converter, so the .ico is assembled here
# and committed with the PNGs. macOS builds its .icns from them at package
# time, because iconutil is always there.
#
# 256x256 is the largest Windows reads, and it is stored PNG-compressed
# inside the .ico; both tools below do that on their own.
ICO="$OUT/$APP_ID.ico"
ICO_SIZES=(16 24 32 48 64 128 256)
ico_inputs=()
for size in "${ICO_SIZES[@]}"; do
	ico_inputs+=("$OUT/${APP_ID}-${size}.png")
done

if command -v icotool >/dev/null 2>&1; then
	icotool -c -o "$ICO" "${ico_inputs[@]}"
elif command -v magick >/dev/null 2>&1; then
	magick "${ico_inputs[@]}" "$ICO"
elif command -v convert >/dev/null 2>&1; then
	convert "${ico_inputs[@]}" "$ICO"
else
	die "need icotool (icoutils) or ImageMagick to build $APP_ID.ico"
fi
note "$APP_ID.ico (${ICO_SIZES[*]})"

say "Wrote ${#ICON_SIZES[@]} icons and $APP_ID.ico to ${OUT#"$REPO_ROOT"/}"
