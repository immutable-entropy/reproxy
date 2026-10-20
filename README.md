# reProxy

A lightweight QA tool for our pre-production web apps. `reproxy` starts a local
proxy (SOCKS5, SOCKS4/4a, HTTP and HTTPS, all on one port) and opens a desktop window that lists every HTTP request your
browser makes through it, with the raw request and response for each.

Scope is deliberate: HTTP/1.x, in the clear or over TLS (see [HTTPS](#https)).
HTTP/2, WebSocket frames and other non-HTTP/1.x traffic are tunnelled through
untouched but not recorded.

## Quick start

```sh
make run                 # build and launch (proxy on 127.0.0.1:7193)
./bin/reproxy -port 8000   # use another port for this session only
./bin/reproxy -help
```

| Flag | Default | |
|---|---|---|
| `-port` | saved setting (7193) | Proxy port for this session; not saved |
| `-config` | `~/.reproxy/config.json` (Windows: `%APPDATA%\reproxy\config.json`) | Settings file; the window layout is saved beside it |
| `-headless` | off | Run with no window: proxy, hub and MCP server only (see [Packaging](#packaging)) |
| `-verbose` | off | Log connection-level detail to stderr |
| `-version` | | Print version and exit |

The listen address can also be changed (and saved) in **File → Settings →
Proxy**. Changing it moves the proxy without dropping connections already
open; if the new address can't be used, the proxy stays where it was and the
dialog shows why. The default host, `127.0.0.1`, keeps the proxy private to
your machine. It has no authentication, so think twice before choosing
`0.0.0.0` or a LAN address.

The window's size and position, both dividers, where the navigation bar and
the Sessions panel sit, and which panels are shown (View menu) are saved on
exit to `ui-state.json` next to the settings file, and restored on the next
launch. Delete that file to get the default layout back.

The **View** menu holds the window's own arrangement. *Navigation* shows the
section names beside the navigation bar's icons and anchors the bar to the
left (the default), top, right or bottom — the same setting as **Settings →
General**, so changing it either way sticks. *Sessions* controls the panel
that holds the Terminal and Agents tabs: *Position* pins it to any of the
window's four edges (the bottom by default) or pops it into a window of its
own, and *Contents* chooses which tabs it holds — with none ticked the panel
is hidden altogether. Pinned to the left or the right it has no room for a
tab's two columns abreast, so they stack until it is dragged wide again. The
panel's own kebab, at its top right, offers the same positions. Last comes
the status bar.

## HTTPS

The proxy decrypts HTTPS so you can record and edit it (needed to test against
`https://` staging). It does this the way Burp and mitmproxy do: on first run
it generates its own certificate authority, kept in `~/.reproxy/ca/`
(`%APPDATA%\reproxy\ca` on Windows), and for each site it mints a certificate
signed by that CA. A browser only accepts those without warnings if it trusts
the CA.

- The **embedded browser** (Tools → Open Browser) trusts the CA automatically
  — it is installed into the browser's certificate store at launch (this needs
  `certutil`, from `libnss3-tools` / `nss`; without it the browser still opens
  but HTTPS shows warnings).
- For **any other browser**, download the CA from **Settings → Traffic Proxy →
  Download CA certificate** and add it to that browser's (or the OS's) trusted
  authorities. The SHA-256 fingerprint shown there lets you verify it.

The proxy does **not** verify staging's own certificate, so internal-CA or
self-signed staging certs work without extra setup. Decryption applies to all
HTTPS; a site using certificate pinning won't connect through it. Non-HTTP/1.x
traffic over TLS (HTTP/2, WebSocket after upgrade) is passed through decrypted
but not recorded.

> Keep `ca-key.pem` private: anyone with it can mint certificates your
> machine's browsers trust. It is written owner-readable only.

## Filtering the request list

The bar above the request list filters it as you type. By default the text is
a plain, case-insensitive substring matched against each request's method,
URL and status — so `api/users`, `stage.example.com` or `500` all narrow the
list. The badge beside **Requests** and the status bar show how many of the
captured requests are shown.

Click the **`.*`** toggle (or press `Ctrl+F` to jump to the field) to switch
to regular expressions, like VS Code's search box. Regex matching is
case-sensitive; prefix `(?i)` to ignore case. A malformed expression turns the
field red and matches nothing until it is fixed.

The list has two densities. The **rows** button at the right of the Traffic
header (`Ctrl+Shift+C`) packs each request onto a single line, or gives them
room again; it shows the density it would switch to. Compact is also enabled
automatically once more than 50 requests have been captured, after which you
can switch back. The choice is remembered between sessions.

## Themes

**Settings → General → Theme** chooses the colour theme: **Light** (the
default), **Dark** (an IDE-style dark theme) or **Matrix** (dark with bright
phosphor green). The theme previews as you pick it and is remembered between
sessions. In the dark themes, 4xx and 5xx status pills keep their amber/red so
errors still stand out.

## Intercepting

**Tools → Interception → Intercept Enabled** (`Ctrl+I`) switches interception
on; the status bar shows *Intercept on* while it is. Interception is always off
when the app starts.

Held messages appear in the request list as a **HELD** pill and open one at a
time, oldest first. **Forward** (top right) sends the message on with your
edits, and **Drop** discards it and closes the browser's connection. Each
action confirms itself: the status bar flashes a short message and the affected
row pulses briefly (green for forward, red for drop). Switching interception off
forwards anything still held, unchanged. Edited flows are marked *edited*, and
the original message is kept with the flow.

In the list, the request **method** is shown as a chip and the **status** as a
tinted pill, so they read by shape as well as colour.

## The request/response editor

Each tab shows a message as structured parts:

- the **start line** (method/path or status) in its own field;
- the **headers** as key/value fields — on a held message you can edit values
  and keys, remove a header (✕), or **+ Add header**;
- the **body** in an IDE-style editor: a line-number gutter, current-line
  highlight, and syntax highlighting. The language is detected from the
  `Content-Type` (JSON, HTML, XML, JavaScript, CSS, or plain) and follows it as
  you edit the header, until you override it with the **Body** selector.

The **Pretty / Raw** toggle switches between this view and the exact raw
message text; it stays visible in both. Non-held messages are shown read-only;
held ones are editable.

Right-click in the body (Pretty or Raw) for the usual cut/copy/paste/select-all
plus a **Tools** submenu of text transformations: Base64 encode/decode and URL
encode/decode. A tool works on the selection, or the word under the cursor if
nothing is selected. In an editable (held) body it replaces the text in place;
in a read-only one it copies the result to the clipboard.

**Settings → Interception** chooses:

- whether to hold requests, responses or both;
- all traffic, or only traffic matching rules. Rules match on host, IP
  address, file extension or content type, and each rule is *Include* or
  *Exclude*;
- whether `Content-Length` is corrected when you change a body's size. Turn
  it off to test deliberately wrong lengths.

Bodies that aren't text (images, gzip) show as a placeholder line. Leave that
line alone and the original bytes are sent; header edits still apply.
Chunked bodies are shown and sent with their chunk framing as-is.

## Replaying requests

**Replay** (top-right of the viewer, for any request) opens the request in a
separate Replay window — a repeater. The request is editable on the left
(same structured editor), the response read-only on the right. **Send**
(or Ctrl+Enter) fires the current request straight at the server and shows the
response with its status, timing and size; edit and resend as often as you
like, then close the window. Replay connects directly to the target (not
through the proxy), so replays don't appear in the capture history. HTTPS
targets are replayed over TLS without verifying the server certificate, like
the proxy.

Each send is recorded in the **Replay History** pane at the bottom of the
window (shown automatically after the first send, newest first). Selecting an
entry loads that request back into the editor and its response beside it, so you
can compare runs or pick an earlier request up, tweak it and send it again. The
window's **View** menu toggles the history pane (`Ctrl+3`) and the status bar.

## Findings

A **finding** is anything the work turns up and you want to keep: a
credential, a leaked field, a response that shouldn't have come back. The
**Findings** section lists them, newest first, each with the type pills that
classify it and the requests it was seen in. A finding you haven't opened yet
is tinted, and the navigation bar's Findings badge counts exactly those, so
the badge and the highlighted rows always agree. Clicking one opens a pane
from the right with its value, notes, details and sightings; the ⇄ beside a
sighting jumps back to that request in Traffic.

There are three ways a finding is recorded:

- **By hand.** Select text in a header or a body — in the Traffic viewer or
  in a Replay tab — and right-click → **Create Finding…**. The window opens
  with the value filled in and the exact spot ("response header
  (Set-Cookie)") already recorded. Add types, notes and any key/value details
  you want to keep with it.
- **By an agent**, through the `create_finding` MCP tool. An agent must
  explain every finding in a note — one without is refused — and what it
  records arrives unviewed, so the badge tells you it is there. Read-only MCP
  clients can't record findings; nobody is asked to approve one, since
  nothing is sent anywhere.
- **By a plugin**, once plugins exist: they will record findings through the
  same path.

Findings are **deduplicated on their value**. Recording a value that is
already a finding adds this sighting to it and merges in any new type or
note, rather than making a second one — so the same key found on twenty
endpoints is one finding with twenty sightings.

They belong to the open engagement and are kept in its `capture.db` beside
the traffic, so they survive restarts and can be queried by other tools.
Clearing the requests (`Ctrl+K`) does **not** clear the findings: each
sighting keeps its own method, URL and location, so a finding still reads
correctly once the traffic it came from is gone.

**Settings → Findings** holds the types: name and colour each, shared by
every engagement. A finding refers to a type by its id, which is taken from
the name when the type is created and then stays put; renaming or recolouring
a type updates every finding that carries it, and removing one leaves its
findings showing the bare id rather than losing the tag.

## Recovered files

Some of the work produces **files** rather than findings: the original,
un-minified source a JavaScript source map carries, a bundle taken apart, an
asset worth a closer look. Right-click a `.map` response in Traffic and
choose **Export recovered sources…** and reProxy writes the application's
source tree under the open engagement (`recovered/<host>/<bundle>/`, 0600
like everything else holding traffic), never outside it however hostile the
paths in the map are.

The **Files** section lists what has been recovered: each file's path, what
it is, how big it is and who recovered it — you, an agent, or a plugin.
Clicking one opens a pane from the right with where it came from (the
request, the Explorer entity, the path on disk) and the file itself in the
same editor the traffic uses, so recovered TypeScript reads as code. The
recovery picker narrows the list to one unpacking, and **Text only** leaves
out the fonts and images a build ships beside its source.

Recovered files belong to the open engagement: their records live in its
`capture.db` beside the findings, so they survive restarts, and clearing the
requests doesn't clear them. A file is deduplicated on its path — recovering
the same path with the same bytes changes nothing — and `reproxy files ls`,
`files cat` and `files recover` do the same work from the command line, while
agents read them with `list_files` and `get_file`.

The point of keeping them in the app rather than in a folder somewhere is
that **every recovered file is offered to the plugins**: a plugin that
recovers a build hands the bytes to reProxy, and the plugins that scan text
for secrets or fingerprint a stack are called on each file as it lands
(`file.on_created`), with the code they already use for response bodies.

## AI agents

**Settings → Agents** adds an agent: pick a vendor (Anthropic, Gemini,
DeepSeek, Qwen, Kimi, or Ollama for a model on your own machine) and paste
its API key. Keys go to `secrets.json`, readable only by you. Agents appear
in the Sessions panel's Agents tab, and reach reProxy through the MCP server
described in [Packaging](#packaging).

An agent can read **everything reProxy captures** — every request and
response that went through the proxy, headers and bodies alike, plus your
findings and the Entity Explorer — and what it reads is sent to the model to
be answered. That means session cookies, passwords, API keys and personal
data, not just the part you are testing. reProxy says so once, before you
add your first agent, and then stays out of the way: be deliberate about
what you put through the proxy while an agent is configured, and sign out of
anything unrelated first.

## Opening a browser

**Tools → Open Browser** (`Ctrl+Shift+B`) starts an installed Chrome,
Chromium, Edge, Brave or Firefox, already pointed at the proxy. No setup is
needed, and nothing is embedded in the app (the same approach as Burp).
The browser gets its own profile in `~/.reproxy/browser/`, kept between
sessions, so your everyday browser is never touched. Its background update
and sync traffic is switched off, and HTTP→HTTPS auto-upgrades are disabled,
so the request list shows your app's traffic.
**Settings → Browser** picks which browser to use (default: the first one
found) or takes a path to any executable. Browsers opened this way are closed
when reProxy exits, including on Ctrl+C or SIGTERM.

A brand-new Chrome profile makes one small plain-HTTP download from
`edgedl.me.gvt1.com` (a security component Chrome won't let flags switch
off). It doesn't recur on later launches.

Snap-packaged browsers (Ubuntu's default Chromium and Firefox) can't use
`~/.reproxy`, so their profile lives in `~/snap/<name>/common/reproxy-profile`.

## Pointing your own browser at it

To use an existing browser instead, configure it by hand. The proxy's one
port (7193 by default) speaks every protocol a browser or tool might use, and
tells them apart from the first byte each connection sends:

| Protocol | Use it as | Notes |
|---|---|---|
| SOCKS5 | `socks5://127.0.0.1:7193` | Preferred: names are resolved by reProxy, so DNS is recorded too. |
| SOCKS4a | `socks4a://127.0.0.1:7193` | Also passes names to reProxy. |
| SOCKS4 | `socks4://127.0.0.1:7193` | Carries only an IPv4 address, so the client resolves names itself. |
| HTTP | `http://127.0.0.1:7193` | `CONNECT` for https URLs (decrypted like a SOCKS tunnel), and plain `GET http://…` requests for http URLs. |
| HTTPS | `https://127.0.0.1:7193` | The same as HTTP, over TLS to the proxy. Its certificate is signed by reProxy's CA, which the client must trust. |

Requests sent to the HTTP proxy name their URL in full; reProxy sends each
to its server in the usual form, with `Host` set from the URL and the
`Proxy-Connection` and `Proxy-Authorization` headers taken out, and records
it as sent. One connection can carry requests for several sites: each is
answered in order. An `https://` URL in such a request (some tools send one
instead of a `CONNECT`) is sent over TLS.

Browsers skip proxies for `localhost` by default, which is exactly what we
test against, so both settings below matter.

**Firefox** (recommended; it keeps its proxy settings per profile):
Settings → Network Settings → Manual proxy configuration →
SOCKS Host `127.0.0.1`, Port `7193`, SOCKS v5, tick *Proxy DNS when using
SOCKS v5*. Then in `about:config` set
`network.proxy.allow_hijacking_localhost` to `true`.

**Chrome / Chromium**: launch a separate profile from the command line:

```sh
google-chrome --user-data-dir=/tmp/reproxy-chrome \
  --proxy-server="socks5://127.0.0.1:7193" \
  --proxy-bypass-list="<-loopback>"
```

`<-loopback>` removes Chrome's built-in localhost bypass.

## Dev container

The dev container installs Qt 6 and Google Chrome and forwards port 7193 to
the host, so you can run `make run` in the container terminal and:

- the window opens on your host desktop through VS Code's X11 forwarding
  (Linux hosts; `DISPLAY` is set for you);
- a **host** browser can use `127.0.0.1:7193` as its proxy (SOCKS5, SOCKS4/4a,
  HTTP or HTTPS).

- **Tools → Open Browser** opens Chrome inside the container, also on your
  desktop, already using the proxy. (The container has no GPU, so the launcher
  adds `--disable-gpu` automatically — without it Chrome's window maps but
  never paints and looks invisible.)

Requests are then made from **inside the container**, so `localhost` means
the container. Apps running in the container work as-is. To reach an app
running on the host, browse to `host.docker.internal:<port>` instead (on Linux
this needs `--add-host=host.docker.internal:host-gateway` in `runArgs`).

## Packaging

`make package` builds an installable package into `dist/`. There is a
packager for each of the four types.

```sh
make package                      # a .deb for this machine
make package TYPE=rpm             # an .rpm (needs rpmbuild)
make package TYPE=mac             # a .dmg (must be run on macOS)
make package TYPE=win             # an installer .exe (needs makensis)
make package TYPE=deb ARCH=arm64  # cross-build (needs a matching Qt 6 toolchain)
```

`packaging/package.sh` resolves the type and hands over to
`packaging/<type>/build.sh`; everything shared (naming, version
normalisation, building the binary, locating icons) lives in
`packaging/common.sh`, so a new packager starts by sourcing it.

The desktop entry, the systemd user unit and the man page are installed
identically by every Linux packager, so they live in `packaging/linux/` and
are staged by one shared function (`stage_linux`). Only the metadata around
them differs — `DEBIAN/control` versus the spec — which is why the two
packages install byte-identical trees apart from their per-format doc files
(Debian's `copyright` and `changelog.Debian.gz`, rpm's `README.md`). The
desktop platforms share none of that layout — an `.app` bundle and a
`%ProgramFiles%` folder are each their own tree — so `packaging/mac/` and
`packaging/win/` stage their own and take only the naming, versions, binary
and icons from `common.sh`.

Versions come from `git describe` and are normalised for the package
manager, which needs a leading digit and no `-`: `v1.2.3-5-gabc1234` becomes
`1.2.3+5+gabc1234`, and an untagged tree becomes `0.0.0+git<hash>`. Note that
a leading digit alone doesn't identify a tag — half of all commit hashes
start with one — so a release is recognised by its leading numeric component.

### The Debian package

```sh
sudo apt install ./dist/reproxy_<version>_amd64.deb
sudo apt remove reproxy    # remove the program
sudo apt purge reproxy     # ...and everything else the package owns
```

It installs:

| Path | |
|---|---|
| `/usr/bin/reproxy` | the binary |
| `/usr/share/applications/reproxy.desktop` | menu entry: searchable, pinnable |
| `/usr/share/icons/hicolor/*/apps/reproxy.*` | icons, 16–512 plus scalable SVG |
| `/usr/lib/systemd/user/reproxy-mcp.service` | the MCP server, per-user, **not** enabled |
| `/usr/share/man/man1/reproxy.1.gz` | `man reproxy` |

Qt 6 is a shared-library dependency rather than something bundled;
`dpkg-shlibdeps` reads the built binary at package time and writes the real
`Depends`, so `apt` pulls Qt from the distribution.

`apt purge` removes everything the package owns, and the maintainer scripts
stop the user service, drop its enablement and refresh the icon and desktop
caches. Per-user state in `~/.reproxy` (settings, the HTTPS interception CA,
engagements, stored API keys) is deliberately left alone — a package must not
delete files from home directories, and silently removing a CA you installed
in a browser would be a nasty surprise. `postrm` prints the `rm -rf
~/.reproxy` to finish the job.

### The RPM package

```sh
sudo dnf install ./dist/reproxy-<version>-1.x86_64.rpm
sudo dnf remove reproxy
rpm -qip ./dist/reproxy-<version>-1.x86_64.rpm   # inspect without installing
```

It installs the same files as the .deb. Building needs `rpmbuild` (the `rpm`
package on Debian/Ubuntu, `rpm-build` on Fedora); `packaging/rpm/reproxy.spec.in`
is the spec template, and the build fails loudly if any placeholder in it is
left unsubstituted.

Qt is required **by soname**, not by package name: the Qt 6 packages are
`qt6-qtbase-gui` on Fedora and `libQt6Gui6` on openSUSE, but both provide
`libQt6Gui.so.6`, which rpmbuild's dependency generator picks out of the
built binary. That makes one .rpm usable on either. Only `hicolor-icon-theme`,
which can't be derived from the ELF, is named explicitly.

RPM has no equivalent of `apt purge`, so `%postun` prints the same
`rm -rf ~/.reproxy` note on final removal.

Two things to know about where you build it:

- **The build host sets the floor.** rpmbuild reads the glibc and Qt symbol
  versions out of the binary, so an .rpm built on a recent Ubuntu asks for a
  recent glibc (`libc.so.6(GLIBC_2.43)`) and won't install on an older
  Fedora. For a release, build it on (or in a container of) the oldest distro
  you intend to support.
- **Release numbers.** The spec's `Release` is `1` by default; set
  `RELEASE=2 make package TYPE=rpm` when the packaging changes but the
  version doesn't.

### The macOS package

```sh
make package TYPE=mac              # arm64 on an Apple silicon Mac
make package TYPE=mac ARCH=x86_64  # Intel
```

It writes `dist/reProxy-<version>-arm64.dmg`: open it and drag **reProxy**
onto the **Applications** shortcut beside it. A `.dmg` runs no scripts, so
everything optional is a command you choose to run afterwards — the `reproxy`
command line and the MCP service below.

```sh
ln -s "/Applications/reProxy.app/Contents/MacOS/reproxy" /usr/local/bin/reproxy
```

**This packager has to run on macOS**, unlike the Linux ones, which only need
a Qt toolchain for the target. `internal/ui` is cgo against Qt 6, so the
binary needs the macOS SDK, and the bundle is assembled with Apple's own
tools (`iconutil`, `codesign`, `hdiutil` — `xcode-select --install`). It
stops with that explanation when run anywhere else.

The bundle:

| Path inside `reProxy.app/Contents` | |
|---|---|
| `MacOS/reproxy` | the binary |
| `Info.plist`, `PkgInfo` | bundle metadata, from `Info.plist.in` |
| `Resources/reproxy.icns` | the icon, 16–1024 |
| `Resources/reproxy-mcp-service` | installs the MCP LaunchAgent |
| `Resources/man/man1/reproxy.1` | `man /Applications/reProxy.app/Contents/Resources/man/man1/reproxy.1` |
| `Frameworks/`, `PlugIns/` | Qt, copied in by `macdeployqt` |

**Qt is bundled, not depended on.** There is no package manager to pull it
from, so `macdeployqt` (from Homebrew's `qt`, or `MACDEPLOYQT=/path/to/it`)
copies the frameworks and the cocoa platform plugin into the bundle and
rewrites the binary's load commands to `@rpath`. Without it the build still
finishes, but the `.app` only runs on a Mac that has Qt installed — so the
packager lists anything still linked from outside the bundle, and says which
of the two it built.

**Signing.** arm64 code must carry at least an ad-hoc signature or the kernel
refuses to run it, so the bundle is always signed; `-` (ad-hoc) is the
default. Everything nested is signed before the bundle around it, because a
bundle's signature covers its contents — and because `macdeployqt` rewriting
load commands invalidates the signatures Qt's frameworks arrive with.

An ad-hoc signature is enough for the machine that built it. On any other
Mac, Gatekeeper quarantines the download: the first open has to come from the
Finder's right-click menu, or `xattr -dr com.apple.quarantine`. To ship
without that, sign with a Developer ID and notarise:

```sh
CODESIGN_IDENTITY="Developer ID Application: You (TEAMID)" \
NOTARIZE_PROFILE=reproxy-notary \
	make package TYPE=mac
```

With `CODESIGN_IDENTITY` set the signature gains the hardened runtime and a
secure timestamp; with `NOTARIZE_PROFILE` (a `notarytool` keychain profile)
the finished `.dmg` is submitted to Apple, waited on, and stapled, so it
opens with no warning and offline.

`SKIP_DMG=1` stops after the `.app`, leaving it in `build/mac/` — the quick
loop when you are changing the bundle itself.

Version numbers get the same treatment as elsewhere, plus one macOS rule:
`CFBundleShortVersionString` must be a dotted number, so it carries the
numeric part alone (`1.2.3`) and the full `git describe` goes in a
`REProxyVersion` key beside it, where `-version` and the Info window can
still show it.

### The Windows package

```sh
make package TYPE=win              # x86-64
make package TYPE=win ARCH=arm64   # Windows on ARM
```

It writes `dist/reProxy-<version>-x86_64-setup.exe`, an NSIS installer that
puts the application in `%ProgramFiles%\reProxy`, adds a Start menu shortcut
(and a desktop one if asked), and registers the Add/Remove Programs entry.
`/S` installs silently, `/D=C:\path` chooses the folder, and
`Uninstall.exe /S` removes it. Installing over an older version offers to
uninstall it first, so Qt DLLs that changed name between releases don't
linger.

**Where to build it.** The installer is assembled by `makensis`, which runs
on Linux as happily as on Windows — but the `.exe` inside it is cgo against
Qt 6. In practice that means MSYS2 (`mingw-w64-x86_64-qt6-base`, `-qt6-tools`
for `windeployqt`, `-nsis` and the toolchain), or a mingw cross toolchain
with a mingw Qt on `PKG_CONFIG_PATH`. Everything missing is named before
anything is built.

**Qt is bundled, not depended on**, as on macOS: `windeployqt` copies the
DLLs and the plugins beside the `.exe`. Without it — the usual state of a
cross-build from Linux — the packager walks the import table with `objdump`
instead, resolving each DLL against Qt's and the toolchain's directories
(`WIN_DLL_DIRS` adds more) and copying the plugin folders a Widgets
application needs by name. That is a best guess, and it says so; if nothing
at all is staged beside the `.exe`, it says that too, because the installed
application would not start.

**The GUI subsystem.** The Windows binary is linked with `-H windowsgui`
(`build_binary` adds it), or Windows would open a console window behind the
app's own. A GUI-subsystem process starts with no standard streams at all,
which would silently swallow `-version`, `-help`, the `plugin` subcommands
and everything a headless run logs — so `cmd/reproxy/console_windows.go`
reattaches to the console it was started from, when there is one, and leaves
redirection the caller set up alone. The one visible quirk is that `cmd.exe`
does not wait for a GUI-subsystem process, so the prompt comes back before
the output does.

**Icon, manifest and version information** are compiled into the `.exe` by
`windres` from `packaging/win/reproxy.rc.in`, and picked up by the Go linker
as a `.syso` placed in `cmd/reproxy` for the build and removed after. The
manifest is what tells Windows the app wants no elevation and is per-monitor
DPI aware (which Qt 6 expects). Without a `windres` for the target
architecture the build carries on and says what the binary will lack; the
shortcuts use `reproxy.ico` either way.

**Signing.** An unsigned installer makes SmartScreen warn about the download
until enough people have installed it anyway. Set `WIN_SIGN_PFX` (and
`WIN_SIGN_PASSWORD`) to sign both the `.exe` and the installer, with
`signtool` on Windows or `osslsigncode` elsewhere; `WIN_TIMESTAMP_URL`
chooses the timestamp server.

Unlike the rpm spec and the macOS plist, `packaging/win/reproxy.nsi` takes
no `@PLACEHOLDER@` substitution: NSIS reads its values from `-D` defines on
the command line, which keeps Windows paths — backslashes, spaces, drive
letters — out of shell quoting. Compiling it by hand stops with a message
naming the define that is missing.

### The MCP service

The MCP server runs inside whichever reProxy process holds the hub role, so
it is up whenever a window is open. To keep it reachable with no window:

```sh
systemctl --user daemon-reload
systemctl --user enable --now reproxy-mcp
```

On macOS it is a LaunchAgent instead, installed by the script in the bundle
(which writes the plist with the path the app was dragged to, so it works
from `/Applications` or anywhere else):

```sh
/Applications/reProxy.app/Contents/Resources/reproxy-mcp-service install
/Applications/reProxy.app/Contents/Resources/reproxy-mcp-service status
/Applications/reProxy.app/Contents/Resources/reproxy-mcp-service uninstall
```

On Windows it is a scheduled task, started at your logon, created by the
script beside the `.exe`:

```bat
"%ProgramFiles%\reProxy\reproxy-mcp-service.cmd" install
"%ProgramFiles%\reProxy\reproxy-mcp-service.cmd" status
"%ProgramFiles%\reProxy\reproxy-mcp-service.cmd" uninstall
```

Everywhere it is a **user** service (a LaunchAgent rather than a
LaunchDaemon, a task of your own rather than a Windows service) rather than
a system one, because all its state is the user's own data directory. It
runs `reproxy -headless`, which starts everything except the window; nothing
in that path touches Qt, so it needs no display. On Linux its output goes to
the journal and on macOS to `~/.reproxy/reproxy-mcp.log`; the Windows task
has no console and so writes no log — run `reproxy -headless -verbose` in a
terminal to watch it.

Both the unit and the LaunchAgent deliberately pass `-port 7293` rather than
letting the proxy take its default. Whichever process starts first takes 7193 and later ones step up
to 7194, so a service sitting on 7193 would quietly swallow traffic from a
browser pointed at it while you watched an empty window.

### Icons

`assets/reproxy.svg` is the source; `assets/icons/*.png` are rendered from it
by `make icons` (needs `rsvg-convert` or ImageMagick) and committed, so
packaging a release needs no SVG renderer. `make icons` also assembles
`assets/icons/reproxy.ico` from those PNGs and commits that, because MSYS2 is
an awkward place to ask for an icon converter; macOS builds its `.icns` at
package time instead, since `iconutil` is always there. The one size not
committed is the 1024 macOS wants for `512x512@2x`, which no Linux icon
theme uses: the mac packager renders it if a renderer is there, upscales the
512 if not, and leaves the entry out if even that fails — an iconset may be
partial.

The icon is a `Re` wordmark over traffic flowing through a node — in on the
left, out on the right — on an accent-blue badge. The lettering is live
`<text>`, so re-rendering wants Inter installed (any sans-serif stands in);
the committed PNGs need nothing. It holds together down to 24px; below that
the badge and its colour do the identifying. Replace the SVG and re-run `make
icons`.

## Development

```sh
make test-core   # proxy/capture/config tests, no Qt build needed, fast
make test        # everything (race detector on the Qt-free packages)
make vet
make deps-check  # dependency soak period (needs the network)
make vulncheck   # govulncheck
make hooks       # once per clone: run deps-check before commits touching go.mod
```

### Dependencies

Every module version in `go.mod` is pinned exactly and checksummed in
`go.sum`, and `go.mod` names the exact Go release to build with (the `go
1.N.P` line; with the default `GOTOOLCHAIN=auto` an older local Go fetches
it). Nothing updates by itself: a version changes only when someone runs
`go get` and commits the result.

New versions soak before we take them. `make deps-check` (`tools/depsoak`)
refuses any required module version, or Go toolchain, first published less
than 14 days ago. "First published" comes from the order of the Go checksum
database (sum.golang.org), not the commit date, which an attacker could
backdate. The only way past it is a line in `deps-soak.allow` naming the
exact version and why it can't wait (a security fix, usually), added after
reading the diff from the version it replaces. depsoak marks a line STALE
once it is no longer needed. The pre-commit hook runs the check whenever
`go.mod`, `go.sum` or `deps-soak.allow` changes.

Building needs Go 1.26.9+, a C++ compiler and Qt 6 development files
(`qt6-base-dev` on Debian/Ubuntu, `brew install qt` on macOS, MSYS2's
`mingw-w64-x86_64-qt6-base` on Windows). The first build compiles the Qt
bindings and takes several minutes; later builds are cached.

### Layout

```
cmd/reproxy/          CLI entry point: flags, logging, launches the app
internal/app/       wires everything together; owns lifecycle; implements ui.Controller
internal/browser/   finds installed browsers, launches them preconfigured (plugin candidate)
internal/capture/   Flow model, thread-safe Store with change events, display helpers
internal/config/    persistent settings and the data directory (~/.reproxy, %APPDATA%\reproxy)
internal/target/    the hosts in scope: one domain, IP or CIDR range each, with path rules
internal/engagement/ an engagement (project): its folder, metadata and the targets in it
internal/archive/   the engagement's traffic on disk (SQLite): the batched writer and the reads
internal/intercept/ interception rules, the hold/forward/drop queue, edit handling
internal/replay/    sends a raw request to a server for the Replay tool
internal/findings/  what the work turned up: the findings and the types that classify them
internal/files/     what the work recovered: the record of each file, its bytes under the engagement
internal/tlsca/     the CA and per-host leaf certificates for HTTPS interception
internal/proxy/     the proxy (SOCKS5, SOCKS4/4a, HTTP, HTTPS) and the HTTP-aware relay that records, decrypts and holds traffic
internal/ui/        Qt (miqt) UI, the only package that uses cgo/Qt
  messageview.go    structured request/response editor (headers + body)
  codeeditor.go     IDE-style body editor (gutter, current line)
  editormenu.go     editor right-click menu: the Tools submenu and Create Finding
  findingspane.go   the Findings section: the list, its filters and the sliding detail pane
  filespane.go      the Files section: the recovered files, and one of them beside the list
  findingdialog.go  the Create/Edit Finding window
  replaywindow.go   the Replay tool window (edit + resend)
  syntax.go         syntax highlighting per language
internal/texttools/ pure text transformations for the editor Tools menu
internal/filter/    request-list filter matching (plain substring and regex)
  assets/theme.qss  the whole visual theme
assets/             application icon: reproxy.svg and the PNGs rendered from it
packaging/          `make package`
  package.sh        dispatcher: resolves the type, hands over to deb/rpm/win/mac
  common.sh         shared naming, version normalisation, binary build, staging, icons
  mkicons.sh        re-renders assets/icons/*.png from the SVG
  linux/            files both Linux packagers install: desktop entry, unit, man page
  deb/              the Debian packager: control, maintainer scripts, copyright
  rpm/              the RPM packager: build.sh and the spec template
```

Dependencies only point one way: `ui` reads from `capture` and talks to the
app through the `ui.Controller` interface; `proxy` writes to `capture`,
consults `intercept`, and uses `tlsca` to decrypt HTTPS; none of `proxy`,
`capture`, `intercept` or `tlsca` knows about Qt. New features (filtering,
search, export, replay) should follow that split.

### How the proxy handles traffic

Each tunnel (a SOCKS or HTTP `CONNECT`, or an HTTP proxy's plain requests
to one site) runs two loops, one per direction. Each loop reads one
message's start line and headers, asks the interceptor whether to hold it,
then either:

- **streams it straight through**: headers, then the body as it arrives, so
  server-sent events and large downloads aren't delayed. A copy is recorded
  up to 16 MiB per message. Or:
- **holds it**: reads the complete message, waits for Forward or Drop, then
  sends the (possibly edited) bytes.

Messages are forwarded byte-for-byte unless edited: chunked framing, header
order and casing are kept. Interim `1xx` responses pass straight through, so
`Expect: 100-continue` works. After a `101 Switching Protocols` (WebSockets),
or on anything that doesn't parse as HTTP/1.x (TLS), the tunnel becomes a
plain byte pipe and stops recording. Responses are paired with requests in
order, which is how HTTP/1.1 works on one connection. With interception off,
the check is a single atomic load per message.

Known limitation: if the browser gives up on a request while it's held, the
flow stays held until you forward or drop it; the send then fails.

~~

## License

reProxy is released under the MIT License; see [LICENSE](LICENSE).
