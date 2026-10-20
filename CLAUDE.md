# reProxy: guidance for Claude sessions

## Project direction (set by the project owner; treat as fixed)

These describe where the project is going. They are **not** a backlog: don't
build toward them unless asked. Do keep every change consistent with them.

1. **Two layers: core and plugins.**
   - **Core** is written in Go. **Intercepting, editing and replaying**
     requests and responses are *the* focus of core. Other features may join
     core eventually, but those three come first.
   - **Core must keep overhead on every intercept as low as possible.** All
     traffic goes through core, so the per-request cost of core code matters
     more than anything else in the project.
   - **Plugins** are where customisation lives. They are written in
     **TypeScript**. They can be added from a local directory or from a URL.
     **No plugins are bundled with the app:** every plugin is loaded from a
     repository, by default the reproxy-plugins repo
     (`github.com/immutable-entropy/reproxy-plugins`, checked out in the
     devcontainer at `/workspaces/reproxy-plugins`). **Author new plugins
     there, not in this repo**; its `CLAUDE.md` is the plugin authoring
     guide.
   - Plugins generally **target specific intercepts**, and they are expected
     to be less performant than core. The performance budget applies to core;
     a plugin's cost should only be paid by the traffic that plugin targets.
   - **Current phase: building out core.** Once core is stable, new
     functionality (especially automated QA testing) is built **as plugins**,
     not inside the app.

2. **Installable package with persistent state.** The end goal is an
   installable package that keeps its state between sessions on the local
   system: `%APPDATA%` on Windows, `~/.reproxy` on macOS, BSD and Linux.

3. **LLM pairing.** The end state is a chat window for working with an LLM
   that can watch the traffic flowing through the app. As tools and plugins
   are added, the LLM should be able to monitor, intercept, edit, test and
   replay traffic, with the aim of finding flaws that must be fixed before our
   web apps go to production.
   - **Scope rule:** by default, the LLM only sends requests to the same
     domain the original request targeted. This will probably be widened
     later for cross-domain apps, where sticking to one domain could miss
     issues. Never widen it without an explicit decision from the project
     owner. Enforced in Go by `internal/scope` (same registrable domain;
     exact host for IPs and single-label names), not in the prompt.
   - **This work is now underway, and it is being built in Go as core**
     (decided by the project owner), not deferred to the plugin layer: the
     agent runtime and its MCP server are the machinery that drives
     intercept/edit/replay, so they belong beside it. The pieces:
     `internal/agent` (vendors, chat sessions, the provider runtime),
     `internal/mcp` (the tool registry and stateless MCP server, spec
     2026-07-28), `internal/instance` + `internal/hub` (one agent sees every
     running reproxy process: each serves its instance over loopback HTTP and
     one holds a lock-file hub role), `internal/approval` (actions awaiting
     the user's yes/no), `internal/focus` (what the user is looking at, so
     "this request" resolves: a short timestamped history, so it resolves
     to what was focused when the message was sent; agents read it only by
     calling `get_user_focus`, which the in-app chat answers as of the
     message's time, and it is never put in the prompt), and `internal/secrets` (API keys and the MCP
     bearer token, 0600). The MCP server listens on 3917 by default
     (forwarded from the devcontainer for external agents).

## What that means when writing code

- Before adding a feature, decide whether it is **core or plugin**. It belongs
  in core only if it is intercept/edit/replay machinery (or something core
  can't work without). If it's unclear, ask.
- Expose core capabilities as **Go APIs that don't depend on Qt** (as
  `capture.Store` does), so that the UI, plugins and future LLM tooling all
  drive the same operations. `internal/ui` presents and calls; it doesn't hold
  logic.
- Keep all persistent state under `config.DataDir()` (goal 2). Don't scatter
  files across other locations.
- Settings have two layers. The user's own are `config.Config`
  (`config.json`, Settings → Global). An engagement can override some of
  them in its folder's `settings.json` (`config.EngagementSettings`,
  Settings → Engagement): today the upstream chain and
  `Targets.Required`, which replace the global value when set, and
  suppressions, which are **added** to the global list. What is in force is
  `Config.Effective(EngagementSettings)`; `app` works it out again on either
  Apply and whenever an engagement opens or closes
  (`internal/app/engagementsettings.go`), so code that acts on a setting an
  engagement can override must read the effective value, not `a.cfg`. A new
  override is a pointer field (nil: inherit) plus a "Use the global
  setting" box on its Engagement page. Upstream passwords for either layer
  live in `secrets`, merged by proxy ID (`UpdateProxyPasswords`), never in
  the engagement folder. The footer shows the chain in force.
- Plugin hooks must be **opt-in per intercept**: a plugin declares what it
  targets, and core matches that in Go. Traffic that no plugin targets must
  never enter the TypeScript runtime.

## Working in this repo

- **Commit each change as it is made**, to the current branch, one commit
  per change (set by the project owner), rather than leaving work
  uncommitted for later.
- **Commit messages describe the change and nothing else** (set by the
  project owner): no mention of AI, assistants, models or the agent that
  made the change, and no `Co-Authored-By` or "Generated with" lines.
- `make test-core` runs the Qt-free packages with `-race`: fast, no display
  needed. `make test` runs everything (`make test-ui` is the Qt half).
  GitHub Actions runs both on every push to any branch
  (`.github/workflows/test.yml`: the core job with gofmt and vet, and the
  Qt job in an Ubuntu 26.04 container like the devcontainer), and once
  both pass, `make deps-check` and `make vulncheck` (in the Qt container,
  since govulncheck loads the Qt packages). Actions are pinned to a
  release's commit SHA, published 14 or more days earlier. Don't add `-race` to the Qt packages:
  it forces a separate rebuild of the Qt bindings that takes about 20 minutes.
- **Dependencies soak for 14 days.** Never run `go get -u`, `go get
  …@latest` or anything else that takes the newest version: pick a version
  first published 14 or more days ago and run `make deps-check`
  (`tools/depsoak`, which dates versions by their sum.golang.org record,
  not the forgeable commit time), which the pre-commit hook runs whenever
  `go.mod`/`go.sum` change. A version too young to pass goes in
  `deps-soak.allow` with a reason, and only once its diff from the version
  it replaces has been read; that is the user's call, so ask. `go mod tidy`
  picks the latest version of a module a new import needs: pin it back
  with `go get mod@version` before committing. Prefer the standard library
  to a new dependency, since every module compiled in runs on users'
  workstations.
- The first build compiles the miqt Qt bindings (about 20 minutes, then
  cached). Run long builds in the background.
- Dependencies point one way: `ui` → `browser`/`capture`/`config`/`crawl`/`intercept`/`syntaxcheck`/`editassist`/`sitemap`/`entities`/`files`/`plugin`/`target`/`engagement`/`terminal`;
  `tlsca` → `atomicfile` only (crypto otherwise);
  `atomicfile`, `elide` and `randid` are leaf helpers that depend on
  nothing at all, internal or external, and everything may import them.
  Anything that writes a file in the data directory goes through
  `atomicfile.Write`/`WriteJSON` (temp file plus rename, with the mode
  stated at the call site — `atomicfile.Private` for anything holding
  traffic, findings or keys), never `os.WriteFile` and never its own
  temp-and-rename. Anything that shortens text for a screen, a log or a
  model goes through `elide` rather than slicing a string: captured
  traffic is arbitrary bytes, so cutting it at a byte offset splits a rune
  and yields invalid UTF-8 — `elide.Runes`/`Tail` count runes, for a
  width on screen; `elide.Bytes`/`Prefix` count bytes, for how much goes
  to a model or a log. Anything that needs a random ID or secret calls
  `randid.Hex`/`Fill`: `crypto/rand.Read` is documented never to return
  an error (it crashes the program instead), so there is nothing to
  handle, and a fallback would be unreachable code that weakened the ID
  if it ever ran;
  `app` → everything but `ui`; `proxy` → `capture`/`intercept`/`websocket`; `dns` → `capture`/`intercept`;
  `config` → `intercept`/`upstream`; what interception holds is now
  configured per target (`target.Intercept`, surfaced through `target.Match`
  and the app's `intercept.UserHoldFunc`), so `intercept` keeps no match
  rules of its own — the master on/off toggle and the per-message edit stage
  are all it holds; `proxy` → `tlsca` (the HTTPS-interception CA).
  `upstream` (the chain of HTTP/HTTPS/SOCKS4/4a/5 proxies outbound
  connections are routed through, Settings → Outbound Traffic) depends on nothing
  internal; the proxy and `replay` reach it through a dial function, so
  neither imports it. `app` builds the dialer once per settings change,
  taking each proxy's password from `secrets`.
  `browser` → `tlsca` (for the CA's name when installing trust); it isn't
  core and should move into a bundled plugin once plugins exist. `texttools`
  (editor Tools-menu transforms), `filter` (request-list filter matching,
  plain substring and regex), `syntaxcheck` (body-editor validation for
  JSON, XML, HTML, JavaScript and CSS) and `editassist` (body-editor typing
  assists and tag completion) depend on nothing internal and are
  plugin candidates too. `sitemap` (the Explorer section's tree) →
  `entities`: it only arranges the entities store's rows into a tree (and
  filters it by entity type and content kind), so it is presentation and a
  plugin candidate as well. The UI rebuilds the tree only while the
  Explorer is on screen, never on the proxy path. Only `internal/ui`
  imports Qt.
- `entities` (Qt-free core) → `capture`/`bodyview` is the engagement's
  **Entity Explorer** (the UI section was once the Map): typed entities —
  `Domain` (registrable domain, eTLD+1), `Host` (either a name host — a
  DNS name or IP address on its own, key `//name`, which DNS facts such as
  `HostAttrs.Addresses` belong to — or an origin, `scheme://name[:port]`,
  under its name host) and `URL` (scheme, host and templated path,
  `/users/12` → `/users/{id}`, with an OpenAPI-style summary merged in as
  traffic arrives). They nest as a strict chain (`Entity.Parent`: URL →
  origin → name host → Domain; an IP's or single-label name's host is a
  root); putting a URL in the store puts its ancestors there too, and a
  request's hit is counted on every one of them. A host can also be put
  in without a URL (`Store.DiscoverHost`, a plugin's `entities.addHost`),
  with the addresses it resolves to, each a host of its own. Schema 9
  rekeyed schema 7's address-only hosts to `//name`, and the store moves
  older archives' origins under their name hosts on first open. Every entity has
  type-specific attributes (`DomainAttrs`, `HostAttrs`, `URLAttrs`) and a
  key/value store (`KV`, edited by the user, `set_entity_kv` and a
  plugin's `entities.set`). An entity no traffic reached is *discovered*
  (`Visited()` false) and drawn faded. Each target's domain or address is
  an entity (`Store.SyncTargets`, seeds worked out by `app.entitySeeds`;
  a target-made entity nothing came of goes with its target). Every
  create/update/delete is an event (`Created`/`Updated`/`Deleted`), which
  the plugin Dispatcher turns into `<type>.on_*` and `entity.on_*` hooks.
  Created and updated hooks only run for entities in a target
  (`Event.InTarget`: a target covers the host, or it is a target's seed)
  unless `config.Targets.AllEntityHooks` (Settings → Targets) is on
  (`Dispatcher.SetAllEntities`); deleted hooks always run.
  The Explorer is drawn from it, **never from the traffic**: its rows live
  in the engagement's `capture.db` (tables `entities`, `entity_flows`,
  `entity_links`, schema owned by `archive`, handed a `*sql.DB` like
  `findings`; schema 7 renamed schema 6's `endpoints` tables and the store
  completes the hierarchy on first open), `key` is UNIQUE and every write
  an upsert, so there is one row per entity whoever writes it, and like
  findings they survive Clear Requests (the Explorer's clear button empties
  them; a delete takes the subtree). Traffic reaches it through
  `entities.Indexer`,
  a store subscriber that only queues on the proxy's goroutine and parses
  finished flows in batches on its own. A visit only *creates* a URL when
  its answer says the path exists (`Visit.found`: not 404/410, and an
  answer at all), so a probe for `/.env` that comes back 404 maps the
  origin that answered but not the path; a URL already known is visited
  whatever it answers; plugins reach it through
  `Store.Discover` (`entities.addUrl`, whose `from` attributes a URL no
  flow led to, so a lookup made for a target can make a host Related). Discovery is core: every finished flow's response is
  read (`entities.FindURLs`: HTML, CSS, scripts, JSON, robots.txt,
  sitemaps, OpenAPI, and the Location/Link/CSP/CORS headers), and a
  request's Referer/Origin is a link too, on **any** host. Which hosts the
  targets cover reaches the store as an `entities.ScopeFunc` (`app` calls
  `SetScope` at startup and on every target change), so `entities` doesn't
  import `target`. A host no target covers that a covered host refers to
  is **Related** (`Store.Related`, announced once with a `RelatedAdded`
  event, which badges the Targets nav item until the section is visited);
  the Explorer draws `Store.Drawn`, which leaves related hosts out (even
  ones traffic reached) until one is promoted to a target or the header's
  eye asks for them — `Store.Mapped` is the same with them in. Promotion is
  the Targets section's Related pane,
  `internal/ui/relatedpane.go`; hosts ticked there are promoted or
  suppressed together, after a `confirm` (`internal/ui/confirm.go`, the
  shared screen-fitting confirmation), in one write each:
  `target.Store.CreateAll` and `Controller.SuppressHosts`. A host can instead be **suppressed**:
  `config.Targets.Suppressed` (a user setting, so it spans engagements;
  `config` → `target` for its validation) holds `target.Suppression`
  patterns written like target addresses, compiled by
  `target.CompileSuppressions` and handed to the store as an
  `entities.SuppressFunc` (`SetSuppressed`, on startup and whenever
  `ApplySettings` changes the list). A suppressed host's entities are kept;
  it is just never related (nor announced) while suppressed.
  Settings → Targets (`internal/ui/settings_targets.go`) edits, imports and
  exports the list (plain text, one pattern per line, `# note`). An archive upgraded from schema 3 is indexed once on
  its first open (the `entity_meta` backfill marker).
- `terminal` (Qt-free, no internal deps) is the Sessions panel's Terminal tab:
  one pseudo-terminal per session running the user's own shell (`$SHELL`,
  started in the open engagement's folder), a virtual screen of what it has
  written, and `Session.Snapshot`, which copies that screen into plain Go
  values (`Screen`, `Cell`, `Key`) for `internal/ui/terminalview.go` to
  paint — so the emulator's types stop at the package boundary and the UI
  has no terminal logic of its own. A snapshot keeps a colour's *kind*:
  the sixteen a shell names (SGR 30–37 and friends) are names, not values,
  so they are passed on as indices and the view resolves them against the
  theme's `ansi*` palette keys (`terminalANSI`), which is what keeps a
  prompt's blue readable on a dark background; a 256-colour index or a true
  colour is the program's own choice and is drawn as given. The pty is `creack/pty` and the
  emulation `charmbracelet/x/vt` (scrollback, alt screen, true colour).
  The environment those shells get is `app.terminalEnv` (`Options.Env`):
  the running binary's own directory first on `PATH`, so `reproxy` in one
  of these shells is the copy the user is looking at whether or not one is
  installed system wide, plus `REPROXY_INSTANCE` and `REPROXY_DATA_DIR`, so
  a command typed there acts on *that* window's process and its data
  directory rather than on whichever process holds the hub role.
  It is in Go because a plugin can't fork a process, and because reproxy's
  own command line is run through these sessions; it is **not** on any
  traffic path — it never touches the capture store, the interceptor or the
  proxy's goroutine. One mutex per session covers the emulator: the pty
  reader writes it, the UI reads it for a snapshot, and the only unlocked
  call is the blocking read of what the emulator has to send back, which is
  why a session is ended by killing its shell and waking that read rather
  than by closing the emulator. `app` owns the `terminal.Manager`
  (`Terminals()`, closed with the app) so sessions outlive any one view.
- Reading a body (core, Qt-free): `bodyview` depends on nothing internal and
  is what makes a message readable — it undoes chunked framing and gzip,
  deflate, brotli and zstd content coding, pretty-prints JSON and XML, and
  puts an edited body back into the framing the headers describe.
  `capture` → `bodyview` (`DisplayText`, which decodes but does not reformat:
  it is what agents read over MCP) and `intercept` → `bodyview`
  (`EditableView` for the editor's Pretty view, `EditableText` for Raw, and
  `ApplyEdit` for the way back). It runs when someone looks at or edits a
  message, never on the proxy's goroutine: traffic is recorded and relayed
  exactly as it arrived, and a body that is only read goes back out byte for
  byte.
- Scope and the engagement container (both Qt-free core): `target` (the
  Targets section — one domain, IP or CIDR range per target, plus the path
  rules that decide what is harvested, and whether interception holds the
  target's requests/responses) depends on nothing internal, and the
  proxy consults the compiled `target.Set` on the hot path through a
  `proxy.ScopeFunc`, so `proxy` doesn't import it. A whole program's scope
  can be imported rather than typed: `target.ParseBurpScope` reads a Burp
  Suite scope file (which is also the shape HackerOne hands a program's
  scope out in) and returns the targets it describes, and `Store.AddAll`
  adds them in one write and one notification, skipping hosts the list
  already holds. The mapping is deliberate and never widens scope: entries
  that differ only by protocol or port collapse into one target, because a
  target matches a host on every port; a host pattern becomes a Domain or a
  Wildcard when it is plainly one and stays a Regex otherwise (padded with
  `.*` when Burp's pattern was unanchored, since reproxy anchors a Regex);
  a file pattern becomes Allow rules with a `Deny .*` under them, which the
  first-match-wins order of `Set.Match` turns into "only these paths"; and
  an exclude becomes a Deny on the target that holds the host, or — for a
  host the includes don't name — a target of its own that denies, ordered
  above them, which is how one host comes back out of a wildcard. Anything
  a target cannot hold (an odd port, a simple-mode prefix's path) goes in
  its notes, and an entry that cannot be imported is reported rather than
  dropped. `target.Adopt` is what readies an imported target — the identity
  and defaults `New` gives one the user typed, keeping what the import
  worked out — so the Targets section and the New Engagement wizard agree.
  `engagement` → `target`:
  an engagement is what other tools call a project — a folder holding
  `engagement.json` and `targets.json` (and, later, captured traffic and
  findings), so it can be saved, copied and reopened. Opening one points the
  single `target.Store` at that engagement's `targets.json`; with none open
  the store uses the data directory's own `targets.json`. The engagement
  that was open is recorded in `engagement-state.json` and reopened on the
  next launch.
- `findings` (Qt-free core) is what the work turned up: a finding has a
  value, the types that classify it, notes, a key/value store and a link to
  every place it was seen — a request (`Link.FlowID`), or a recovered file
  (`Link.Location` is `InRecoveredFile` and the file's path is in `Detail`,
  made with `RecoveredFileLink`; a `Draft` carries extra such links in
  `Links`, which `spa-recon` uses to point an exposed-source-map finding at
  the source it unpacked). It depends on nothing internal (it is handed
  a `*sql.DB`), and `config` → `findings` because the type catalog is a user
  setting. Findings are deduplicated on their value: `Store.Record` merges a
  draft into the finding that already holds it rather than adding a second,
  which is the one entry point for all three routes — the user's right-click
  (`internal/ui/editormenu.go`), an agent's `create_finding` tool, and a
  plugin's `findings.record`, told apart by `findings.Source`. Nothing in it
  touches the proxy goroutine, so it commits synchronously like
  `target.Store`. A plugin's `finding_types` join the catalog through
  `DeclareTypes` while it runs and are never written to the settings file.
  A finding whose value is a secret in itself carries `Hidden` (set by its
  recorder — a plugin's `hidden`, the agent tool's `hidden`, the dialog's
  checkbox, or `Store.SetHidden`): the UI then draws `SummaryMasked()` in
  the list and `Masked()` in the detail pane until the user clicks the eye
  beside the value (`internal/ui/findingdetail.go`). It conceals the value
  on screen and nothing else — it is still stored, searched and handed to
  agents and plugins, because a value an agent can't read is one it can't
  test — and a merge can hide a finding but never un-hide it.
  While the findings reviewer (`app/findingsreview.go`, an agent session)
  is enabled, a new finding it will review is held back
  (`Finding.AwaitingReview`, through `Store.SetReviewHold`): not unread and
  not in the badge until its review settles it — unread if it scores
  `Notable` (both scores at or above their floors), read if not — or the
  turn that set out to review it ends without scoring it, which releases it
  unread (`Store.ReleaseReview`), so a reviewer that can't run never hides
  a finding.
- `files` (Qt-free core) is what the work **recovered**: the record of every
  file pulled out of the traffic and written under the engagement — the
  original source a map carries, a bundle taken apart, an asset kept for a
  closer look. It depends on `atomicfile`, `elide` and `randid` only, and is
  handed a `*sql.DB` (schema 15's `files` table, owned by `archive`) plus
  the directory the bytes live in (`Store.Use(db, root)`,
  `<engagement>/recovered/`, the data directory's own while no engagement is
  open). **The bytes are on disk, the record is in the database**: the rows
  carry the relative path, size, sha256, media type, kind, who recovered it
  and the flow, host and entity it came from, and the file is read back on
  demand (`Store.Read`, bounded) — never for a file that is merely listed,
  since one recovery can be thousands of files. Like findings it survives
  Clear Requests (`flow_seq` is not a foreign key) and commits
  synchronously: nothing here is on the proxy's goroutine.
  **Why it is core:** producing the bytes is a plugin's business — core
  writes them because a plugin has no filesystem — but a recovered file is
  something *other* plugins act on, so core owns the object and the event. A
  plugin that scans text for secrets hooks `file.on_created` and reads
  recovered client source with the code it already has for response bodies,
  instead of every recovering plugin carrying its own copy of every scanner.
  Files are deduplicated on their path, and **recovering the same path with
  the same bytes changes nothing and raises no event**, so a recovery can be
  run again without setting every scanner off a second time; different bytes
  replace the file and raise `Updated`. A whole recovery goes in through
  `Store.RecordAll`, which gives every file one **batch** and ends with a
  single `BatchDone` — the per-file events are thousands and a plugin's
  queue holds 1024, so the batch hook is what a plugin flooded by them
  waits for, and `Select(Query{Batch: …})` has the whole recovery whether or
  not its events survived. Every path is untrusted whoever cleaned it
  (`files.SafeRel`, applied again by `Record`): a source map's
  "../../etc/passwd" is written inside the recovered files or not at all.
  The routes in are the traffic list's "Export recovered sources…"
  (`app.ExportSourceMap` → `internal/sourcemap`, which now only *reads* a
  map), the `export_source_map` tool, `reproxy files recover`, and a
  plugin's `files.add`/`files.addAll`; the routes out are the Files section
  (`internal/ui/filespane.go`, between Explorer and Findings), the
  `list_files`/`get_file` tools, the `files` namespace and the file hooks.
- Plugins (all Qt-free; the authoring guide is `README.md` and `CLAUDE.md`
  in the reproxy-plugins repo, `/workspaces/reproxy-plugins`):
  `plugin/manifest` (plugin.json, the hook catalog, match filters compiled
  for Go) and `plugin/jsrt` (esbuild's Go API bundles a plugin's main.ts in
  a sandboxed resolver — its own files and the SDK only — and goja runs it,
  one runtime and goroutine per plugin, under a per-turn time budget)
  depend on nothing internal; `plugin/sdk` embeds the TypeScript SDK
  (`reproxy`, `reproxy/testing`); `plugin` →
  `capture`/`intercept`/`findings`/`files`/`target`/`entities`/`replay`/`bodyview`/`filter`/`cli`
  runs them (Manager, Dispatcher, the intercept stage, the host functions
  the SDK wraps) and must not import `config`, `proxy`, `upstream`,
  `secrets` or `archive`: `app` injects dial functions, secrets and the
  user's overrides. `plugin/plugintest` (→ `archive`/`config`) runs a
  plugin's `*.test.ts` against a throwaway engagement, for
  `reproxy plugins test`. The hot-path rules:
  the Dispatcher's store subscribers start with one atomic load of an
  immutable routing table (nil while nothing is routed); traffic hooks are
  **observe-only and asynchronous** — matched in Go, then queued
  non-blocking on the plugin's bounded queue, dropped and counted when
  full, with every JS value (body decoding included) built on the plugin's
  goroutine; only `intercept.on_*_pending` waits for plugins, through the
  `intercept.StageFunc` installed by the Manager only while some plugin
  targets it, and only on messages interception already holds. A plugin's
  traffic through the proxy authenticates with SOCKS5 username/password
  (`plugin.OriginLabel`, a per-process secret held by `app`), which the
  proxy's `OriginFunc` turns into `Source: plugin` and `Flow.Origin`;
  plugins skip their own traffic unless a target sets `self`, and sends
  past `plugin.MaxDepth` are refused. Plugins may reach any host (the LLM's
  `internal/scope` rule doesn't apply to them) but always through the
  upstream chain: `http.send` refuses a direct send when `Deps.Dial` is
  nil rather than let `replay` fall back to dialling directly.
  **Actions** (`manifest.Action`, plugin.json's `actions`) are run by the
  user, never by core: `Manager.Actions(flow)` lists the running plugins'
  actions whose filter (`Match.CompileAction`: a traffic filter that
  accepts every source by default) accepts the flow, and
  `Manager.RunAction` calls one with `{ action, flow }` (`plugin/actions.go`),
  each turn under the turn budget and the whole call under `actionBudget`.
  The traffic list's context menu (`internal/ui/flowactions.go`) shows
  them and runs one off the UI thread; nothing is added to the proxy's path. Status and setting overrides live in `config.Plugins`
  (secret settings in `secrets`); `plugin.json` is never written. Nothing
  is embedded in the binary: the Manager finds plugins only in the data
  directory's `plugins/`. Plugins are installed there
  through `internal/plugin/registry` (Qt-free, no internal deps): it lists and
  downloads plugins from a GitHub repository over an injected Getter (the app
  sends it through the upstream chain), finding a `plugin.json` at the repo
  root, a top-level directory, or under the repo's `src/`/`plugins/`. The app
  (`AvailablePlugins`/`InstallPlugin`) validates a download with
  `plugin.Inspect`, installs it disabled (so downloaded code never runs until
  the user enables it), and reloads. Each install records where it came
  from in the plugin's `.reproxy-origin.json` (`registry.Origin`). Each
  time the Plugins window opens it lists the repositories again and badges
  the plugins a repository has a newer `manifest.Version` of
  (`AvailablePlugins` sets `Update`); nothing updates until the user clicks
  Update, and `UpdatePlugin` keeps the plugin's status and settings. A
  plugin with no origin file was put in the folder by hand: it is still
  offered the newer version, marked `InstalledByHand` so the bar and the
  confirmation say the files being replaced are the user's own (which is
  how a copy made during development is updated, and why nothing is
  replaced without a yes). A plugin that came from another repository is
  left alone: that repository is its source, and the same name elsewhere is
  a different plugin. Repositories live in
  `config.Plugins.Repositories` (default: the reproxy-plugins repo). The UI
  for all of it is the Plugins window (`internal/ui/pluginswindow.go`,
  opened from the puzzle piece above the navigation bar's cog), laid out
  like Settings through the shared `runPagedDialog`: a Plugins section
  (installed plugins, their settings and logs, and the interception
  permissions) and a Repositories section.
- `cli` (Qt-free, no internal deps) is the **command line**:
  `reproxy <namespace> <action> --flags`, where the namespace is a core
  object (`findings`, `targets`, `plugins`…) or one a plugin declared.
  It holds the shape, not the commands: the `Registry` core commands are
  registered in (`app.newCommandRegistry`, the commands themselves in
  `internal/app/cli_*.go`, one file per area), the argument parsing, the
  help (`cli.Table` for listings, `cli.Fields` for walking a settings
  struct by dotted path), and the reserved core namespace table (`namespaces.go` — the whole list, and
  `cli.Reserved` is what keeps a plugin out of one; the singular is an
  alias of each, so `finding ls` is `findings ls`). **A plugin's namespace
  is its own name**: `reproxy wordpress <action>`, so plugin.json declares
  actions, not namespaces, and two plugins can no more share a namespace
  than a directory. **A command runs in a
  running reproxy process, not beside one**: the client finds the hub
  through `hub.Locate` and the shared bearer token, posts to the hub's
  `/hub/cli` route (`cli.HubHandler`, wired in `app`), and the instance
  `--instance` names — the hub's own by default — answers it through
  `instance.Instance.RunCommand`, against its live stores, open engagement
  and loaded plugins. Nothing is parsed twice: only the namespace, the
  action and the raw arguments travel, because the flags a command takes
  are declared where it runs. The exceptions are the author tools
  (`cli.LocalCommands`: `plugins validate|test|types <dir>`), which work on
  a directory of the user's and so run in the command line's own process —
  declared in the registry all the same, so help lists everything in one
  place. `reproxy shell` is the same commands typed without the program's
  name. Each command is an adapter and nothing more: it reads its flags,
  calls the method the window or an agent would call (`instance.Local` for
  traffic, interception and Replay; `target.Store`, `findings.Store`,
  `entities.Store`, `crawl.Crawler`, `agent.Manager`, `App.ApplySettings`
  for the rest) and renders the answer — so a command that needs something
  core can't do yet means adding a method to the package that owns the
  object, not logic here. Destructive commands (`flows clear`,
  `entities clear`, `crawl start`, `mcp token --regenerate`, a scope
  import) refuse without `--yes`: a command line answers once, so there is
  nobody to prompt. A flag given as `-` is what was piped in
  (`Request.Stdin`, read by the client only when an argument is `-`, so an
  ordinary command never blocks on a pipe), which is how a message, a body
  or a scope file reaches a command running in another process.
  `REPROXY_DATA_DIR` points the client at another data directory, as the
  app's own `-config` does.
  `reproxy shell` is the same commands typed without the program's name.
  On a terminal it reads with `cli.Editor` — a small line editor (history
  on the arrow keys, completion on Tab, the usual Ctrl-A/E/U/K/W) written
  here rather than taken from a library, since the only dependency it
  needs is `charmbracelet/x/term` for raw mode, which the terminal
  emulator already links in. Raw mode is on only while a line is being
  read, never while a command runs, so output, signals and pipes behave as
  they do outside the shell; piped input skips the editor entirely. What
  Tab offers comes from the instance: `reproxy help --json` answers with a
  `cli.Catalogue` (every command and its flags), which the shell fetches at
  startup and again after a `plugins` command, so a plugin enabled in the
  shell completes on the next line. History is kept in
  `<datadir>/shell-history`, 0600 through `atomicfile.Private`, because a
  command line can carry a token or a finding's value.
  A plugin declares commands in plugin.json (`manifest.Command`), and
  `plugin.Manager` implements `cli.PluginCommands`: core parses the flags
  the plugin declared and calls its function once, on the plugin's own
  goroutine under the action budget, so nothing enters the TypeScript
  runtime for a command nobody typed. A plugin named for a core namespace
  may not declare commands (`checkNamespace`, called from `reconcile`): it
  doesn't run at all, with that as its problem, rather than running with
  its commands missing.
- `replay` → `capture`: besides its own session history, every replay send
  (the user's or an agent's) is recorded in the `capture.Store` as a flow
  whose `Source` is `replay` or `agent` (`Sessions.RecordTo`, wired in
  `app`), so the Traffic list, the archive and agents' flow listings show
  all traffic, not just what came through the proxy. Replays aren't sent
  through the proxy, so interception never holds them (only the DNS
  lookups made on the way, see `dns`).
- `crawl` (Qt-free core) → `capture`/`entities`/`replay` is **Autocrawl**:
  it fetches (GET only) every URL a target covers, follows the links
  `entities.ResponseURLs` finds in each response, and records each request
  in the `capture.Store` as a flow whose `Source` is `crawl` (tagged CRAWL
  in the request list; plugins' traffic hooks see it by default, but not
  `replay.on_sent`). It is given its dial function and has no way to
  connect on its own: `app` hands it `dialResolved`, so every request
  goes through the upstream chain in force, with the lookup made by `dns`.
  Scope is `target.Set.Match` (host and path rules) through a
  `crawl.ScopeFunc`, so `crawl` doesn't import `target`; a link to a host
  no target covers is kept as *waiting*, and `Rescope` (subscribed to the
  target store) queues it once one does, which is how promoting a related
  host extends a running crawl. The limits are strict: request starts are
  spaced at least 1/rate apart with no burst (across workers, stops and
  starts), and at most `Threads` (≤ `crawl.MaxThreads()`, one per CPU)
  are in flight. Every request carries `Settings.UserAgent` (reProxy's
  own, `crawl.DefaultUserAgent`, when empty; the window and `--user-agent`
  offer `crawl.UserAgents` or the user's own text, refused if it holds a
  control character, since it goes into a header line as it stands).
  It crawls what it is led to, not what might be there:
  the URLs the Explorer knows, every origin's `crawl.SitemapPaths`
  (robots.txt, sitemap.xml), and whatever those turn up, recursively.
  Guessing at `crawl.APIDocPaths` (the usual OpenAPI/Swagger locations)
  is `Settings.APIDocs`, off by default, and stops on an origin once a
  description is found there (`docs_found`); a guess is told from a
  discovered URL by being at one of those paths with no `found_on`, and
  guesses still queued are dropped when the toggle is off. The window
  chooses the targets crawled (`Settings.ExcludedTargets`, the ones left
  out, so a new target is crawled by default); a URL under a left-out
  target waits like one no target covers. `entities` reads API descriptions in JSON
  or YAML (`go.yaml.in/yaml/v3`, only for text that starts like one),
  served as JSON/YAML or as octet-stream under a `.json`/`.yaml` path, and
  gives a templated path filled in from its parameters' examples (detail
  `openapi:… example`). The crawler never requests a `{…}` path, nor a
  described path with no GET. An origin that can't be connected to costs
  one request: the rest of its queue is marked failed untried. Its state
  (`crawl_urls`, `crawl_meta`: every URL's state, the window's settings
  and whether the start warning was dismissed) is in the engagement's `capture.db` (schema
  10, owned by `archive`; it survives Clear Requests), and it follows the
  engagement like `findings` (`app.followEngagement` stops the crawl and
  waits for its requests before the archive moves). "Only crawl new URLs"
  skips what earlier runs crawled; off, Start queues everything again.
  The UI is the Autocrawl window (`internal/ui/crawlwindow.go`, a
  top-level window, not a dialog, so traffic, findings and the Explorer
  stay usable), opened from the crawler button left of the Browser
  button. Start first asks the user to confirm (`confirmCrawl`, modal to
  that window only) that a crawl is noisy and can stress a server, unless
  they ticked "Don't show this again", which holds for the engagement.
- `dns` (Qt-free core) → `capture`/`intercept` makes every name lookup a
  connection needs, so DNS is seen and interceptable like HTTP. The proxy
  calls it once per connection, before dialling, through a
  `proxy.ResolveFunc` (so `proxy` doesn't import it); Replay and plugins'
  direct sends dial through `Resolver.Dialer`, which reads who the lookup is
  for from `capture.WithSource`. With no upstream chain it resolves on this
  machine; when the chain can carry names
  (`upstream.Dialer.ResolvesRemotely`, false for a plain-SOCKS4 last hop)
  the name is passed on and the upstream resolves it, so DNS goes through
  the chain. A lookup in scope (`target.Set.MatchHost`: the name, or the
  addresses it resolved to; path rules don't apply) is a flow with
  `Protocol: dns` whose messages are text (`QUERY name DNS` / `DNS NOERROR`
  with `A:`/`AAAA:` lines). A target holds lookups only with
  `Intercept.DNS`, the query when it holds requests and the answer when it
  holds responses; plugins never see them (no traffic hooks, no intercept
  stage), nor do the Explorer, Replay or agents' replays. Answers are cached for
  a minute (flushed when the targets or the chain change), so a cache hit
  costs a map lookup. The request list hides lookups unless View → Show DNS
  Lookups (or the DNS toggle in the Traffic panel's header) is on; a held
  one always shows.
  The browsers reproxy launches are set to resolve nothing themselves (no
  DNS prefetch, DoH or non-proxied WebRTC).
- `archive` → `capture`: the engagement's `capture.db`, a SQLite database
  holding every flow and its messages (and, in their own tables, the
  engagement's findings: `archive` owns the schema and its version and hands
  `findings` the `*sql.DB`, so one package owns that file's schema). An
  upgrade step is the schema as it was then, never the current constant —
  schema 3 creates the findings tables from `findingsSchemaV3` and schema 8
  adds `findings.hidden` to them — so one chain of steps serves an archive
  whatever version it starts from.
  `finding_flows.flow_seq` is deliberately not a foreign key — Clear Requests
  empties `flows` and must not take the findings with it, so each sighting
  carries its own method/URL snapshot. SQLite is chosen over a key/value
  store because an archive has to be queryable (and openable by other
  tools), and it is `modernc.org/sqlite` — pure Go, no cgo — so core still
  cross-compiles for packaging. **It must never do I/O on the proxy's
  goroutine:** `Archive.Record` is a store subscriber that only queues
  (~400ns, no allocation), and one background goroutine coalesces the queue
  and commits each batch in a transaction. A flow's row is keyed by the
  archive's own `seq`, which `Recent` hands back as the in-memory flow ID,
  so reloading an engagement and then editing a flow updates its row instead
  of adding one. `app` owns the lifecycle: an `archive.Recorder` is
  subscribed to the store once at startup and the database under it is
  swapped when the engagement changes (`app.followEngagement`).
- The agent/MCP packages (all Qt-free core) layer like this:
  `scope`, `secrets` and `focus` depend on nothing internal;
  `instance` → `capture`/`intercept`/`replay`/`focus`/`filter`/`cli`
  (for `RunCommand`, the command line's way in);
  `hub` → `instance`; `mcp` → `instance`/`hub`/`scope`/`approval`/`focus`;
  `agent` → its providers only (and, once wired, `mcp`);
  `mcp` → `findings`, `files` and `entities` as well, for the findings
  tools, the file tools (`list_files`, `get_file`) and the entity tools (`list_entities`, `get_entity`, `add_url`,
  `set_entity_kv`; `list_flows` takes an `entity` key). They act on the
  local process's stores rather than through an `Instance`, because
  findings and entities belong to the engagement one process has open. A
  tool that records a finding (or writes an entity) sets `Writes` rather
  than `Mutates`: a read-only caller is refused, and nobody is asked to
  approve it (nothing leaves the machine; the Findings badge is the
  notification);
  `ui` and `app` also → `agent`. Keep agent/MCP work off the proxy hot
  path: subscribe to the store/interceptor, never add per-message cost.
- Any code or message text the user views or edits goes in a `codeEditor`
  (`internal/ui/codeeditor.go`), configured with `editorOptions` (title,
  language selector, validation, typing assists), rather than a bare
  `QPlainTextEdit`, so every editor gets the same features.
- To check the UI headlessly, use Xvfb rather than opening windows on the
  user's desktop:
  `Xvfb :99 -screen 0 1440x900x24 &`, `DISPLAY=:99 ./bin/reproxy &`, then
  `DISPLAY=:99 xwd -root -silent | convert xwd:- shot.png`. Popups and dialogs
  come out black in root captures; capture them with
  `xwd -id $(xdotool search --name '^Settings$')`. Keyboard input needs
  `xdotool windowfocus <id>` first (there is no window manager), after which
  shortcuts and menu navigation (`alt+t`, arrow keys) work.
- Don't use `pkill -f <pattern>` where the pattern also appears in your own
  command line: it kills the shell running the command.
