#!/usr/bin/env bash
# Builds the Windows installer. Driven by packaging/package.sh; run it as
#
#   make package TYPE=win              # x86-64
#   make package TYPE=win ARCH=arm64   # Windows on ARM
#
# Installed per machine (the layout is in reproxy.nsi):
#
#   %ProgramFiles%\reProxy\reproxy.exe                 the application
#   %ProgramFiles%\reProxy\<Qt DLLs>, platforms\, ...  from windeployqt
#   %ProgramFiles%\reProxy\reproxy.ico                 the shortcuts' icon
#   %ProgramFiles%\reProxy\reproxy-mcp-service.cmd     the MCP scheduled task
#   A Start menu shortcut, an optional desktop one, and the Add/Remove entry
#
# Where to run it: the installer itself is built by makensis, which runs on
# Linux as well as Windows, but the .exe inside it is cgo against Qt 6. That
# means MSYS2 (mingw-w64-x86_64-qt6-base, -nsis and the toolchain) on
# Windows, or a cross toolchain with a mingw Qt on Linux. Everything is
# checked before anything is built, and what is missing is named.
#
# Environment:
#   WINDEPLOYQT         path to windeployqt, if it isn't on PATH or in QT_DIR
#   WIN_DLL_DIRS        extra directories (colon-separated) to resolve DLLs
#                       from when windeployqt isn't available
#   WINDRES             path to windres, for the icon and version resource
#   WIN_SIGN_PFX        code-signing certificate (.pfx); signs the .exe and
#                       the installer with signtool or osslsigncode
#   WIN_SIGN_PASSWORD   its password
#   WIN_TIMESTAMP_URL   timestamp server (default: DigiCert's)

set -euo pipefail
source "$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)/common.sh"

HERE="$REPO_ROOT/packaging/win"
need makensis

# --- target -----------------------------------------------------------------

# Windows architecture names are neither Go's nor Debian's.
case "${ARCH:-}" in
	"")             WIN_ARCH="$(uname -m)" ;;
	amd64|x86_64)   WIN_ARCH=x86_64 ;;
	arm64|aarch64)  WIN_ARCH=aarch64 ;;
	*) die "unsupported --arch '$ARCH' for win (expected amd64 or arm64)" ;;
esac

case "$WIN_ARCH" in
	x86_64)  GOARCH=amd64; TOOL_PREFIX=x86_64-w64-mingw32- ;;
	aarch64) GOARCH=arm64; TOOL_PREFIX=aarch64-w64-mingw32- ;;
	*) die "unsupported machine architecture '$WIN_ARCH' (expected x86_64 or aarch64)" ;;
esac

# MSYS2 reports MINGW64/UCRT64 and friends in MSYSTEM; anywhere else this is
# a cross-build, which needs a toolchain pointed at by CC/PKG_CONFIG_PATH.
if [ -z "${MSYSTEM:-}" ]; then
	note "cross-building for windows/$GOARCH: needs CGO_ENABLED=1,"
	note "CC=${TOOL_PREFIX}gcc and a mingw Qt 6 on PKG_CONFIG_PATH"
fi

VER="$(upstream_version)"
read -r V_MAJOR V_MINOR V_PATCH <<<"$(numeric_version)"
# VERSIONINFO and the installer's VIProductVersion both want four numbers.
VERSION_QUAD="$V_MAJOR.$V_MINOR.$V_PATCH.0"
COPYRIGHT="$(date +%Y) $APP_VENDOR. $APP_LICENSE licensed."

STAGE="$BUILD_DIR/win/stage"
RES="$BUILD_DIR/win/res"

say "Packaging $APP_NAME $VER ($WIN_ARCH)"

# winpath prints a path the way the makensis and windres being used expect
# it: MSYS2 ships native Windows builds of both, which make nothing of
# /c/Users, so paths handed to them go through cygpath.
winpath() {
	if command -v cygpath >/dev/null 2>&1; then
		cygpath -w "$1"
	else
		printf '%s' "$1"
	fi
}

# --- resources --------------------------------------------------------------

# The icon, the manifest and the version information are compiled into the
# .exe by the Go linker, which picks up any .syso sitting in the main
# package. windres is what turns a .rc into one.
rm -rf "$BUILD_DIR/win"
mkdir -p "$STAGE" "$RES"

install -m 0644 "$(icon_ico)" "$RES/$APP_ID.ico"

sed \
	-e "s|@BUNDLE_ID@|$APP_BUNDLE_ID|g" \
	-e "s|@VERSION_QUAD@|$VERSION_QUAD|g" \
	-e "s|@SUMMARY@|$APP_SUMMARY|g" \
	"$HERE/$APP_ID.manifest.in" > "$RES/$APP_ID.manifest"

# The .rc names its two files without a directory, and windres is run from
# the directory holding them, so no Windows path ever reaches the compiler.
sed \
	-e "s|@ICON_FILE@|$APP_ID.ico|g" \
	-e "s|@MANIFEST_FILE@|$APP_ID.manifest|g" \
	-e "s|@VERSION_COMMA@|$V_MAJOR,$V_MINOR,$V_PATCH,0|g" \
	-e "s|@RAW_VERSION@|$(raw_version)|g" \
	-e "s|@APP_ID@|$APP_ID|g" \
	-e "s|@APP_NAME@|$APP_NAME|g" \
	-e "s|@SUMMARY@|$APP_SUMMARY|g" \
	-e "s|@VENDOR@|$APP_VENDOR|g" \
	-e "s|@COPYRIGHT@|$COPYRIGHT|g" \
	"$HERE/$APP_ID.rc.in" > "$RES/$APP_ID.rc"

if grep -q '@[A-Z_]*@' "$RES/$APP_ID.rc" "$RES/$APP_ID.manifest"; then
	die "unsubstituted placeholder: $(grep -ho '@[A-Z_]*@' "$RES/$APP_ID.rc" \
		"$RES/$APP_ID.manifest" | sort -u | tr '\n' ' ')"
fi

# A windres has to emit COFF for the architecture being built. The prefixed
# one always does; the bare one on PATH targets the machine it runs on, so
# it is only trusted when that is the target.
find_windres() {
	if [ -n "${WINDRES:-}" ]; then printf '%s' "$WINDRES"; return; fi
	if command -v "${TOOL_PREFIX}windres" >/dev/null 2>&1; then
		command -v "${TOOL_PREFIX}windres"; return
	fi
	if [ "$WIN_ARCH" = "$(uname -m)" ] && command -v windres >/dev/null 2>&1; then
		command -v windres; return
	fi
	printf ''
}

SYSO=""
WINDRES_BIN="$(find_windres)"
if [ -n "$WINDRES_BIN" ]; then
	say "Compiling resources with $(basename "$WINDRES_BIN")"
	( cd "$RES" && "$WINDRES_BIN" -i "$APP_ID.rc" -o "rsrc.syso" -O coff ) ||
		die "windres failed on $APP_ID.rc"
	# The Go linker only looks for .syso files inside the package being
	# built, so it has to be put there and taken away again. The _windows_
	# suffix keeps it from affecting any other build in the meantime.
	SYSO="$REPO_ROOT/cmd/$APP_ID/rsrc_windows_$GOARCH.syso"
	if [ -e "$SYSO" ]; then
		die "${SYSO#"$REPO_ROOT"/} already exists; refusing to overwrite it"
	fi
	trap 'rm -f "$SYSO"' EXIT
	install -m 0644 "$RES/rsrc.syso" "$SYSO"
	note "icon, manifest and version information"
else
	note "no windres for $WIN_ARCH: the .exe gets no icon, manifest or version"
	note "information (the shortcuts still use $APP_ID.ico). Set WINDRES to one."
fi

# --- build ------------------------------------------------------------------

EXE="$STAGE/$APP_ID.exe"
build_binary windows "$GOARCH" "$EXE"

# Nothing else needs the .syso, and leaving it behind would quietly put this
# version's resources into the next build.
if [ -n "$SYSO" ]; then
	rm -f "$SYSO"
	trap - EXIT
fi

# --- Qt ---------------------------------------------------------------------

# windeployqt copies the Qt DLLs and the plugins an application needs next to
# the .exe, which is how a Windows application carries its runtime: there is
# no package manager to depend on.
find_windeployqt() {
	if [ -n "${WINDEPLOYQT:-}" ]; then printf '%s' "$WINDEPLOYQT"; return; fi
	local name
	for name in windeployqt6 windeployqt; do
		if command -v "$name" >/dev/null 2>&1; then command -v "$name"; return; fi
	done
	if [ -n "${QT_DIR:-}" ] && [ -x "$QT_DIR/bin/windeployqt.exe" ]; then
		printf '%s' "$QT_DIR/bin/windeployqt.exe"; return
	fi
	printf ''
}

# qt_bin_dirs lists, one per line, the directories a DLL shipped with the
# application could come from: Qt's own, the toolchain's, and anything the
# caller added. A DLL found in none of them is a Windows system DLL.
qt_bin_dirs() {
	local dirs=() libdir dir
	if [ -n "${WIN_DLL_DIRS:-}" ]; then
		local IFS=:
		for dir in $WIN_DLL_DIRS; do
			dirs+=("$dir")
		done
	fi
	if [ -n "${QT_DIR:-}" ]; then
		dirs+=("$QT_DIR/bin")
	fi
	if [ -n "${MINGW_PREFIX:-}" ]; then
		dirs+=("$MINGW_PREFIX/bin")
	fi
	# Qt's DLLs sit in bin/ beside the libdir pkg-config reports (mingw
	# keeps the import libraries in lib/ and the DLLs in bin/).
	if libdir="$(pkg-config --variable=libdir Qt6Core 2>/dev/null)" && [ -n "$libdir" ]; then
		dirs+=("$libdir" "$(dirname "$libdir")/bin")
	fi
	[ ${#dirs[@]} -gt 0 ] || return 0
	printf '%s\n' "${dirs[@]}"
}

# copy_imports copies every DLL an executable imports that can be found in
# the Qt/toolchain directories, then does the same for those DLLs. Anything
# not found there is a Windows system DLL and is left alone. It is what
# stands in for windeployqt on a machine that has no windeployqt -- the
# normal state of a cross-build -- and it is also why the plugin directories
# below are copied by name: no import table mentions them.
copy_imports() {
	local exe="$1" objdump="$2" dir dll src item
	local -a queue=("$exe")
	local i=0

	# The queue grows as DLLs are copied, so it is walked by index: each
	# new arrival is examined in turn, and a DLL already staged is never
	# queued twice.
	while [ "$i" -lt "${#queue[@]}" ]; do
		item="${queue[$i]}"
		i=$((i + 1))
		while IFS= read -r dll; do
			if [ -z "$dll" ] || [ -e "$STAGE/$dll" ]; then
				continue
			fi
			src=""
			while IFS= read -r dir; do
				[ -d "${dir:-}" ] || continue
				# Import tables are not consistent about case
				# (KERNEL32.DLL next to Qt6Core.dll) and Windows does
				# not care, so neither can the lookup -- which matters
				# here because this is the path a Linux cross-build
				# takes.
				src="$(find "$dir" -maxdepth 1 -iname "$dll" -print -quit 2>/dev/null)"
				if [ -n "$src" ]; then
					break
				fi
			done < <(qt_bin_dirs)
			if [ -z "$src" ]; then
				continue   # not ours to ship: a system DLL
			fi
			install -m 0644 "$src" "$STAGE/$dll"
			queue+=("$STAGE/$dll")
		done < <("$objdump" -p "$item" 2>/dev/null |
			awk '/DLL Name:/ {print $3}')
	done
}

copy_qt_plugins() {
	local plugins="$1" sub
	# A Qt Widgets application will not start without the platform plugin;
	# the others are what this UI draws with. windeployqt works the rest
	# out from the binary, which objdump cannot.
	for sub in platforms styles imageformats tls; do
		[ -d "$plugins/$sub" ] || continue
		install -d "$STAGE/$sub"
		find "$plugins/$sub" -maxdepth 1 -name '*.dll' -exec \
			install -m 0644 {} "$STAGE/$sub/" \;
	done
}

WDQ="$(find_windeployqt)"
if [ -n "$WDQ" ]; then
	say "Bundling Qt with $(basename "$WDQ")"
	LOG="$BUILD_DIR/win/windeployqt.log"
	"$WDQ" --release --no-translations "$(winpath "$EXE")" > "$LOG" 2>&1 || {
		tail -20 "$LOG" >&2
		die "windeployqt failed; full log in ${LOG#"$REPO_ROOT"/}"
	}
else
	OBJDUMP=""
	for candidate in "${TOOL_PREFIX}objdump" objdump; do
		if command -v "$candidate" >/dev/null 2>&1; then
			OBJDUMP="$(command -v "$candidate")"; break
		fi
	done
	[ -n "$OBJDUMP" ] || die "neither windeployqt nor objdump found.
       One of them has to say which Qt DLLs to ship beside the .exe:
       install Qt's tools (MSYS2: mingw-w64-x86_64-qt6-tools) and point
       WINDEPLOYQT at it, or install binutils for ${TOOL_PREFIX%-}."

	say "Bundling Qt by walking the import table ($(basename "$OBJDUMP"))"
	note "windeployqt was not found, so this is a best guess at the runtime"
	copy_imports "$EXE" "$OBJDUMP"

	PLUGINS=""
	for q in "${TOOL_PREFIX}qmake6" qmake6 qmake; do
		if command -v "$q" >/dev/null 2>&1; then
			PLUGINS="$("$q" -query QT_INSTALL_PLUGINS 2>/dev/null || true)"
			if [ -n "$PLUGINS" ]; then
				break
			fi
		fi
	done
	if [ -n "$PLUGINS" ] && [ -d "$PLUGINS" ]; then
		copy_qt_plugins "$PLUGINS"
	else
		note "no qmake to locate Qt's plugins: the installed app may not start"
		note "without <install dir>\\platforms\\qwindows.dll. Set QT_DIR."
	fi
fi

DLLS="$(find "$STAGE" -name '*.dll' | wc -l | tr -d ' ')"
note "$DLLS DLLs, $(du -sh "$STAGE" | cut -f1) staged"

# Nothing above fails when it finds no Qt: windeployqt is absent, or the
# qmake on PATH belongs to the host's own Qt rather than a mingw one, and
# the walk quietly turns up nothing to copy. Say so, because an installer
# without the Qt DLLs produces an application that will not start.
if [ "$DLLS" -eq 0 ]; then
	note "no DLLs were staged beside the .exe: unless Qt 6 is already on the"
	note "target machine, the installed application will not start. Point"
	note "WINDEPLOYQT at a windeployqt, or WIN_DLL_DIRS at the mingw Qt's bin."
fi

# --- staging ----------------------------------------------------------------

say "Staging files"

# The shortcuts point at the .ico rather than the .exe, so the icon is right
# even when windres was not available to put one in the binary.
install -m 0644 "$(icon_ico)" "$STAGE/$APP_ID.ico"

sed \
	-e "s|@APP_ID@|$APP_ID|g" \
	-e "s|@APP_NAME@|$APP_NAME|g" \
	-e "s|@TASK_NAME@|$WIN_TASK_NAME|g" \
	-e "s|@MCP_PORT@|$MCP_PORT|g" \
	-e "s|@SERVICE_PROXY_PORT@|$SERVICE_PROXY_PORT|g" \
	"$HERE/$SERVICE_ID-service.cmd.in" > "$STAGE/$SERVICE_ID-service.cmd"

if grep -q '@[A-Z_]*@' "$STAGE/$SERVICE_ID-service.cmd"; then
	die "unsubstituted placeholder in $SERVICE_ID-service.cmd: $(grep -ho \
		'@[A-Z_]*@' "$STAGE/$SERVICE_ID-service.cmd" | sort -u | tr '\n' ' ')"
fi

# Windows reads .cmd files line by line as it runs them, so they must have
# CRLF endings or a trailing argument ends up with a stray CR.
awk '{sub(/\r$/, ""); printf "%s\r\n", $0}' "$STAGE/$SERVICE_ID-service.cmd" \
	> "$STAGE/$SERVICE_ID-service.cmd.crlf"
mv "$STAGE/$SERVICE_ID-service.cmd.crlf" "$STAGE/$SERVICE_ID-service.cmd"

note "$APP_ID.ico, $SERVICE_ID-service.cmd"

# --- signing ----------------------------------------------------------------

# Without a signature SmartScreen warns about the download until enough
# people have installed it anyway. signtool is the Windows tool;
# osslsigncode does the same job when the installer is built on Linux.
sign_file() {
	local file="$1"
	[ -n "${WIN_SIGN_PFX:-}" ] || return 0
	local ts="${WIN_TIMESTAMP_URL:-http://timestamp.digicert.com}"

	local pass=()
	if command -v signtool >/dev/null 2>&1; then
		if [ -n "${WIN_SIGN_PASSWORD:-}" ]; then
			pass=(/p "$WIN_SIGN_PASSWORD")
		fi
		# MSYS2 rewrites arguments that look like paths, which is every
		# one of signtool's /flags.
		MSYS2_ARG_CONV_EXCL='*' signtool sign /fd SHA256 /tr "$ts" /td SHA256 \
			/f "$(winpath "$WIN_SIGN_PFX")" \
			${pass[@]+"${pass[@]}"} \
			"$(winpath "$file")" >/dev/null ||
			die "signtool failed on $(basename "$file")"
	elif command -v osslsigncode >/dev/null 2>&1; then
		if [ -n "${WIN_SIGN_PASSWORD:-}" ]; then
			pass=(-pass "$WIN_SIGN_PASSWORD")
		fi
		osslsigncode sign -h sha256 -ts "$ts" \
			-pkcs12 "$WIN_SIGN_PFX" \
			${pass[@]+"${pass[@]}"} \
			-in "$file" -out "$file.signed" >/dev/null ||
			die "osslsigncode failed on $(basename "$file")"
		mv "$file.signed" "$file"
	else
		die "WIN_SIGN_PFX is set but neither signtool nor osslsigncode is installed"
	fi
	note "signed $(basename "$file")"
}

if [ -n "${WIN_SIGN_PFX:-}" ]; then
	say "Signing"
	sign_file "$EXE"
fi

# --- installer --------------------------------------------------------------

mkdir -p "$DIST_DIR"
OUT="$DIST_DIR/$APP_NAME-$VER-$WIN_ARCH-setup.exe"
rm -f "$OUT"

say "Building $(basename "$OUT")"

# Every value the script needs is a define rather than a substitution, so
# Windows paths never go through sed. STAGE_GLOB carries the separator the
# makensis in use understands.
STAGE_WIN="$(winpath "$STAGE")"
case "$STAGE_WIN" in
	*\\*) STAGE_GLOB="$STAGE_WIN\\*.*" ;;
	*)    STAGE_GLOB="$STAGE_WIN/*.*" ;;
esac

LOG="$BUILD_DIR/win/makensis.log"
MSYS2_ARG_CONV_EXCL='*' makensis -NOCD -V2 \
	"-DAPP_ID=$APP_ID" \
	"-DAPP_NAME=$APP_NAME" \
	"-DSUMMARY=$APP_SUMMARY" \
	"-DVERSION=$VER" \
	"-DVERSION_QUAD=$VERSION_QUAD" \
	"-DVENDOR=$APP_VENDOR" \
	"-DCOPYRIGHT=$COPYRIGHT" \
	"-DHOMEPAGE=$APP_HOMEPAGE" \
	"-DREG_KEY=$WIN_REG_KEY" \
	"-DTASK_NAME=$WIN_TASK_NAME" \
	"-DMCP_PORT=$MCP_PORT" \
	"-DSERVICE_CMD=$SERVICE_ID-service.cmd" \
	"-DICON_FILE=$(winpath "$RES/$APP_ID.ico")" \
	"-DSTAGE_GLOB=$STAGE_GLOB" \
	"-DOUT_FILE=$(winpath "$OUT")" \
	"$(winpath "$HERE/$APP_ID.nsi")" > "$LOG" 2>&1 || {
		tail -30 "$LOG" >&2
		die "makensis failed; full log in ${LOG#"$REPO_ROOT"/}"
	}
[ -f "$OUT" ] || die "makensis reported success but produced no installer"

sign_file "$OUT"

say "Done: ${OUT#"$REPO_ROOT"/} ($(du -h "$OUT" | cut -f1))"
note "install:  run it, or silently: $(basename "$OUT") /S [/D=C:\\path]"
note "remove:   Settings > Apps, or \"%ProgramFiles%\\$APP_NAME\\Uninstall.exe\" /S"
note "service:  \"%ProgramFiles%\\$APP_NAME\\$SERVICE_ID-service.cmd\" install"
if [ -z "${WIN_SIGN_PFX:-}" ]; then
	note "unsigned: SmartScreen will warn on another machine until the"
	note "installer is signed (set WIN_SIGN_PFX) and has some reputation."
fi
