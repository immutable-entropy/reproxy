#!/usr/bin/env bash
# Builds an .rpm for Fedora/RHEL/openSUSE. Driven by packaging/package.sh;
# run it as
#
#   make package TYPE=rpm
#
# It installs the same tree as the .deb (stage_linux in ../common.sh); only
# the metadata differs. See reproxy.spec.in for the spec template.

set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/common.sh"

HERE="$REPO_ROOT/packaging/rpm"
need rpmbuild

# RPM's Release field. Bump it when the packaging changes but the version
# does not.
RELEASE="${RELEASE:-1}"

# --- target -----------------------------------------------------------------

# RPM architecture names differ from both Go's and Debian's.
case "${ARCH:-}" in
	"")                  RPM_ARCH="$(uname -m)" ;;
	x86_64|amd64)        RPM_ARCH=x86_64 ;;
	aarch64|arm64)       RPM_ARCH=aarch64 ;;
	armv7hl|armhf|arm)   RPM_ARCH=armv7hl ;;
	i686|i386|386)       RPM_ARCH=i686 ;;
	*) die "unsupported --arch '$ARCH' for rpm (expected x86_64, aarch64, armv7hl or i686)" ;;
esac

case "$RPM_ARCH" in
	x86_64)  GOARCH=amd64 ;;
	aarch64) GOARCH=arm64 ;;
	armv7hl) GOARCH=arm ;;
	i686)    GOARCH=386 ;;
	*) die "unsupported machine architecture '$RPM_ARCH'" ;;
esac

if [ "$GOARCH" != "$(go env GOARCH)" ]; then
	note "cross-building for $RPM_ARCH; internal/ui needs a matching Qt 6 + cgo toolchain"
fi

VER="$(upstream_version)"

say "Packaging $APP_NAME $VER-$RELEASE ($RPM_ARCH)"

# --- stage ------------------------------------------------------------------

# rpmbuild wants its own tree. BUILDROOT is where %install assembles the
# package; STAGE is what %install copies in, built here so the deb and the
# rpm are assembled by exactly the same code.
TOP="$BUILD_DIR/rpm"
STAGE="$TOP/stage"
rm -rf "$TOP"
mkdir -p "$TOP"/{BUILD,BUILDROOT,RPMS,SOURCES,SPECS,SRPMS} "$STAGE/usr/bin"

BIN="$STAGE/usr/bin/$APP_ID"
build_binary linux "$GOARCH" "$BIN"
chmod 0755 "$BIN"

say "Staging files"
stage_linux "$STAGE"

# %doc in the spec needs the file inside the staged tree.
install -d "$STAGE/usr/share/doc/$APP_ID"
install -m 0644 "$REPO_ROOT/README.md" "$STAGE/usr/share/doc/$APP_ID/README.md"

# --- spec -------------------------------------------------------------------

# rpm wants the changelog date in C locale, and the maintainer without the
# angle-bracket form deb uses ("Name <email>" is fine, but the date must be
# "Day Mon DD YYYY").
CHANGELOG_DATE="$(LC_ALL=C date '+%a %b %d %Y')"

SPEC="$TOP/SPECS/$APP_ID.spec"
say "Generating $(basename "$SPEC")"

# Substitute the @PLACEHOLDERS@. Each value is plain text with no slashes
# that would break the sed expressions, except the homepage, so '|' is used
# as the delimiter throughout.
sed \
	-e "s|@APP_ID@|$APP_ID|g" \
	-e "s|@SERVICE_ID@|$SERVICE_ID|g" \
	-e "s|@VERSION@|$VER|g" \
	-e "s|@RELEASE@|$RELEASE|g" \
	-e "s|@SUMMARY@|$APP_SUMMARY|g" \
	-e "s|@LICENSE@|$APP_LICENSE|g" \
	-e "s|@HOMEPAGE@|$APP_HOMEPAGE|g" \
	-e "s|@RPM_ARCH@|$RPM_ARCH|g" \
	-e "s|@MCP_PORT@|$MCP_PORT|g" \
	-e "s|@MAINTAINER_RPM@|$APP_MAINTAINER|g" \
	-e "s|@CHANGELOG_DATE@|$CHANGELOG_DATE|g" \
	-e "s|@RAW_VERSION@|$(raw_version)|g" \
	"$HERE/$APP_ID.spec.in" > "$SPEC"

if grep -q '@[A-Z_]*@' "$SPEC"; then
	die "unsubstituted placeholder in the spec: $(grep -o '@[A-Z_]*@' "$SPEC" | sort -u | tr '\n' ' ')"
fi

# --- build ------------------------------------------------------------------

say "Building $APP_ID-$VER-$RELEASE.$RPM_ARCH.rpm"

# _stagedir is read by %install. --target pins the architecture so a
# cross-built binary is not labelled with the host's.
rpmbuild -bb "$SPEC" \
	--define "_topdir $TOP" \
	--define "_stagedir $STAGE" \
	--define "_build_id_links none" \
	--target "$RPM_ARCH" \
	> "$TOP/rpmbuild.log" 2>&1 || {
		tail -30 "$TOP/rpmbuild.log" >&2
		die "rpmbuild failed; full log in ${TOP#"$REPO_ROOT"/}/rpmbuild.log"
	}

RPM="$(find "$TOP/RPMS" -name '*.rpm' -type f | head -1)"
[ -n "$RPM" ] || die "rpmbuild reported success but produced no .rpm"

mkdir -p "$DIST_DIR"
OUT="$DIST_DIR/$(basename "$RPM")"
mv "$RPM" "$OUT"

say "Done: ${OUT#"$REPO_ROOT"/} ($(du -h "$OUT" | cut -f1))"
note "install:  sudo dnf install $OUT"
note "remove:   sudo dnf remove $APP_ID"
note "inspect:  rpm -qip $OUT"
