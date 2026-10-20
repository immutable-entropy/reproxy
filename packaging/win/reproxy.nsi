; Installer script for packaging/win/build.sh, compiled with makensis.
;
; Unlike the rpm spec and the macOS plist, nothing here is substituted by
; sed: NSIS takes its own values on the command line (-DNAME=value), which
; keeps Windows paths -- backslashes, spaces, drive letters -- out of shell
; quoting. build.sh passes every symbol the REQUIRE list below names, so
; compiling this by hand stops with a message rather than a broken installer.
;
; What it installs, per machine:
;
;   %ProgramFiles%\reProxy\reproxy.exe            the application
;   %ProgramFiles%\reProxy\<Qt DLLs, plugins>     from windeployqt
;   %ProgramFiles%\reProxy\reproxy.ico            the icon the shortcuts use
;   %ProgramFiles%\reProxy\reproxy-mcp-service.cmd  the MCP scheduled task
;   %ProgramFiles%\reProxy\Uninstall.exe
;   A Start menu shortcut, and a desktop one if asked for
;   The Add/Remove Programs entry

Unicode true

!macro REQUIRE name
	!ifndef ${name}
		!error "missing -D${name}: build this through packaging/win/build.sh"
	!endif
!macroend
!insertmacro REQUIRE APP_ID
!insertmacro REQUIRE APP_NAME
!insertmacro REQUIRE SUMMARY
!insertmacro REQUIRE VERSION
!insertmacro REQUIRE VERSION_QUAD
!insertmacro REQUIRE VENDOR
!insertmacro REQUIRE HOMEPAGE
!insertmacro REQUIRE REG_KEY
!insertmacro REQUIRE TASK_NAME
!insertmacro REQUIRE MCP_PORT
!insertmacro REQUIRE SERVICE_CMD
!insertmacro REQUIRE ICON_FILE
!insertmacro REQUIRE COPYRIGHT
!insertmacro REQUIRE STAGE_GLOB
!insertmacro REQUIRE OUT_FILE

!define UNINST_KEY "Software\Microsoft\Windows\CurrentVersion\Uninstall\${REG_KEY}"

Name "${APP_NAME} ${VERSION}"
OutFile "${OUT_FILE}"
InstallDir "$PROGRAMFILES64\${APP_NAME}"
InstallDirRegKey HKLM "Software\${REG_KEY}" "InstallDir"
; The files go under Program Files and the uninstall entry is machine-wide,
; so the installer needs to be elevated. The app it installs never is: its
; manifest asks for asInvoker.
RequestExecutionLevel admin
SetCompressor /SOLID lzma
ShowInstDetails show
ShowUninstDetails show

VIProductVersion "${VERSION_QUAD}"
VIAddVersionKey "ProductName" "${APP_NAME}"
VIAddVersionKey "ProductVersion" "${VERSION}"
VIAddVersionKey "FileVersion" "${VERSION}"
VIAddVersionKey "FileDescription" "${APP_NAME} installer"
VIAddVersionKey "CompanyName" "${VENDOR}"
VIAddVersionKey "LegalCopyright" "${COPYRIGHT}"

!include "MUI2.nsh"
!include "FileFunc.nsh"   ; GetSize, for the Add/Remove Programs entry

!define MUI_ICON "${ICON_FILE}"
!define MUI_UNICON "${ICON_FILE}"
!define MUI_ABORTWARNING
!define MUI_FINISHPAGE_TEXT "${APP_NAME} is installed.$\r$\n$\r$\nPoint your browser's proxy (SOCKS5, SOCKS4/4a, HTTP or HTTPS) at 127.0.0.1 and the port shown in the window. To read HTTPS, install the CA certificate from Settings.$\r$\n$\r$\nTo keep the MCP server running with no window open, run reproxy-mcp-service.cmd install from the installation folder."

!insertmacro MUI_PAGE_WELCOME
!insertmacro MUI_PAGE_COMPONENTS
!insertmacro MUI_PAGE_DIRECTORY
!insertmacro MUI_PAGE_INSTFILES
!insertmacro MUI_PAGE_FINISH

!insertmacro MUI_UNPAGE_CONFIRM
!insertmacro MUI_UNPAGE_INSTFILES

!insertmacro MUI_LANGUAGE "English"

; --- install ----------------------------------------------------------------

; An earlier version is uninstalled first, so that files it had and this one
; does not -- a Qt DLL that changed name between Qt releases, say -- do not
; linger in the installation folder.
Function .onInit
	SetRegView 64
	ReadRegStr $R0 HKLM "${UNINST_KEY}" "UninstallString"
	StrCmp $R0 "" done
	ReadRegStr $R1 HKLM "${UNINST_KEY}" "InstallLocation"
	StrCmp $R1 "" done

	MessageBox MB_YESNO|MB_ICONQUESTION \
		"${APP_NAME} is already installed in $R1.$\r$\n$\r$\nRemove it before installing this version?" \
		IDNO done
	; _?= keeps the uninstaller in place so ExecWait really waits for it.
	ExecWait '"$R0" /S _?=$R1'
	Delete "$R0"
	RMDir "$R1"
done:
FunctionEnd

Section "${APP_NAME} (required)" SecApp
	SectionIn RO
	SetRegView 64
	SetOutPath "$INSTDIR"
	; STAGE_GLOB is the staged tree plus a wildcard, spelled with whichever
	; path separator the makensis running this understands.
	File /r "${STAGE_GLOB}"

	WriteUninstaller "$INSTDIR\Uninstall.exe"

	CreateShortCut "$SMPROGRAMS\${APP_NAME}.lnk" "$INSTDIR\${APP_ID}.exe" \
		"" "$INSTDIR\${APP_ID}.ico" 0

	WriteRegStr HKLM "Software\${REG_KEY}" "InstallDir" "$INSTDIR"
	WriteRegStr HKLM "Software\${REG_KEY}" "Version" "${VERSION}"

	WriteRegStr HKLM "${UNINST_KEY}" "DisplayName" "${APP_NAME}"
	WriteRegStr HKLM "${UNINST_KEY}" "DisplayVersion" "${VERSION}"
	WriteRegStr HKLM "${UNINST_KEY}" "DisplayIcon" "$INSTDIR\${APP_ID}.ico"
	WriteRegStr HKLM "${UNINST_KEY}" "Publisher" "${VENDOR}"
	WriteRegStr HKLM "${UNINST_KEY}" "URLInfoAbout" "${HOMEPAGE}"
	WriteRegStr HKLM "${UNINST_KEY}" "InstallLocation" "$INSTDIR"
	WriteRegStr HKLM "${UNINST_KEY}" "UninstallString" '"$INSTDIR\Uninstall.exe"'
	WriteRegStr HKLM "${UNINST_KEY}" "QuietUninstallString" '"$INSTDIR\Uninstall.exe" /S'
	WriteRegDWORD HKLM "${UNINST_KEY}" "NoModify" 1
	WriteRegDWORD HKLM "${UNINST_KEY}" "NoRepair" 1

	; Add/Remove Programs shows the size in KB.
	${GetSize} "$INSTDIR" "/S=0K" $0 $1 $2
	IntFmt $0 "0x%08X" $0
	WriteRegDWORD HKLM "${UNINST_KEY}" "EstimatedSize" "$0"
SectionEnd

Section "Desktop shortcut" SecDesktop
	CreateShortCut "$DESKTOP\${APP_NAME}.lnk" "$INSTDIR\${APP_ID}.exe" \
		"" "$INSTDIR\${APP_ID}.ico" 0
SectionEnd

!insertmacro MUI_FUNCTION_DESCRIPTION_BEGIN
	!insertmacro MUI_DESCRIPTION_TEXT ${SecApp} "${SUMMARY}"
	!insertmacro MUI_DESCRIPTION_TEXT ${SecDesktop} "Put a ${APP_NAME} shortcut on the desktop."
!insertmacro MUI_FUNCTION_DESCRIPTION_END

; --- uninstall --------------------------------------------------------------

Function un.onInit
	SetRegView 64
FunctionEnd

Section "Uninstall"
	; The MCP scheduled task runs this binary, so it goes first. It belongs
	; to whoever installed it, and an uninstaller runs elevated as somebody
	; else -- so this is best-effort, and the message at the end says what
	; may be left.
	nsExec::ExecToLog 'schtasks /delete /tn "${TASK_NAME}" /f'
	Pop $0

	Delete "$SMPROGRAMS\${APP_NAME}.lnk"
	Delete "$DESKTOP\${APP_NAME}.lnk"

	Delete "$INSTDIR\Uninstall.exe"
	; Only ever empty a folder that holds the thing we installed, in case
	; $INSTDIR was pointed somewhere shared at install time.
	IfFileExists "$INSTDIR\${APP_ID}.exe" 0 +2
		RMDir /r "$INSTDIR"

	DeleteRegKey HKLM "${UNINST_KEY}"
	DeleteRegKey HKLM "Software\${REG_KEY}"

	; What is left is each user's own data: the settings, the HTTPS
	; interception CA, saved engagements and stored API keys. An installer
	; must not delete those from profiles it does not own -- and removing a
	; CA somebody installed in a browser would be a nasty surprise -- so
	; say what to remove instead of guessing.
	IfSilent done
	MessageBox MB_OK|MB_ICONINFORMATION \
		"${APP_NAME} is removed.$\r$\n$\r$\nYour settings, the HTTPS interception CA, engagements and stored API keys were left in %APPDATA%\${APP_ID}. Delete that folder to remove them, and remove the CA from any browser or certificate store you installed it in.$\r$\n$\r$\nIf you enabled the MCP service as another user, remove its scheduled task ($\"${TASK_NAME}$\") while signed in as that user."
done:
SectionEnd
