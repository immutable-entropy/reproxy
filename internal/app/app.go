// Package app wires configuration, capture, proxy and UI together and owns
// their lifecycle. It implements ui.Controller.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"reproxy/internal/agent"
	"reproxy/internal/approval"
	"reproxy/internal/archive"
	"reproxy/internal/browser"
	"reproxy/internal/capture"
	"reproxy/internal/cli"
	"reproxy/internal/config"
	"reproxy/internal/crawl"
	"reproxy/internal/dns"
	"reproxy/internal/engagement"
	"reproxy/internal/entities"
	"reproxy/internal/files"
	"reproxy/internal/findings"
	"reproxy/internal/focus"
	"reproxy/internal/hub"
	"reproxy/internal/instance"
	"reproxy/internal/intercept"
	"reproxy/internal/mcp"
	"reproxy/internal/plugin"
	"reproxy/internal/plugin/registry"
	"reproxy/internal/proxy"
	"reproxy/internal/replay"
	"reproxy/internal/secrets"
	"reproxy/internal/target"
	"reproxy/internal/terminal"
	"reproxy/internal/tlsca"
	"reproxy/internal/upstream"
)

// Options are the launch parameters gathered by the CLI.
type Options struct {
	ConfigPath string
	// PortOverride, when non-zero, replaces the saved port for this session
	// without being persisted.
	PortOverride int
	Logger       *slog.Logger
}

// App is the running application.
type App struct {
	log        *slog.Logger
	configPath string
	store      *capture.Store
	intercept  *intercept.Interceptor
	proxy      *proxy.Server
	replays    *replay.Sessions
	focus      focus.Tracker
	secrets    *secrets.Store
	instanceID string
	started    time.Time
	node       *hub.Node // nil if joining the group failed
	agents     *agent.Manager
	broker     *approval.Broker // agent actions awaiting the user's decision
	terminals  *terminal.Manager
	// local is this process as an instance: what the command line's own
	// commands read traffic, interception and Replay through.
	local   *instance.Local
	targets *target.Store
	// engagements owns the open engagement: opening one points the target
	// store and the traffic archive at that engagement (see
	// internal/engagement and followEngagement).
	engagements *engagement.Manager
	// recorder writes every captured flow to the open engagement's archive.
	// It subscribes to the store once and is swapped, not resubscribed,
	// when the engagement changes.
	recorder *archive.Recorder
	// findings holds what the work turned up. Its types come from the
	// settings file; the findings themselves live in the open engagement's
	// capture.db, so it follows the engagement like the recorder does.
	findings *findings.Store
	// entities is the engagement's Entity Explorer: its domains, hosts and
	// URLs. The indexer puts the captured traffic in it, off the proxy's
	// goroutine; plugins add what they find, and each target its domain or
	// address. Its rows are in the same capture.db, so it follows the
	// engagement too.
	entities *entities.Store
	indexer  *entities.Indexer
	// files is what the work recovered: the record of every file a plugin,
	// an agent or the user pulled out of the traffic. Its rows are in the
	// same capture.db and its bytes in the engagement's recovered/ folder,
	// so it follows the engagement like the findings do.
	files *files.Store
	// crawler is Autocrawl (see crawl.go). Its state is in the same
	// capture.db, so it follows the engagement too.
	crawler *crawl.Crawler
	// reviewer is the findings reviewer (see findingsreview.go): a background
	// agent session that scores each new finding's confidence. It subscribes
	// to the findings store and the agent manager once, and is told which
	// agent profile reviews whenever the agent configuration changes.
	reviewer *findingsReviewer
	// plugins runs the plugins (see plugins.go). pluginMu serialises
	// handing it settings; pluginSecret is what a plugin's connection
	// through the proxy authenticates with.
	plugins      *plugin.Manager
	pluginMu     sync.Mutex
	pluginSecret string
	// pluginGet, when set, replaces the HTTP getter for plugin repositories
	// (tests inject one so they need no network).
	pluginGet registry.Getter
	// eng is the open engagement's settings overrides (see
	// engagementsettings.go). appliedSuppressed is the suppression list the
	// entities store was last given, so it is only rescanned on a change.
	eng                 config.EngagementSettings
	appliedSuppressed   []target.Suppression
	suppressionsApplied bool
	// lastEngagement is the engagement plugins were last told about: its
	// directory and name.
	lastEngagement [2]string
	// upstreams is the dialer built from Config.Upstream, replaced whenever
	// the chain changes. A nil pointer means connections go out directly.
	// The proxy and Replay both dial through it, so traffic reaches a
	// staging network the same way whichever tool sent it.
	upstreams atomic.Pointer[upstream.Dialer]
	// dns makes the name lookups the proxy, Replay and plugins need, so
	// they are recorded and can be intercepted (see package dns).
	dns *dns.Resolver

	mu       sync.Mutex
	cfg      config.Config
	proxyErr error
	browsers []*browser.Session
	ca       *tlsca.CA
	mcp      *mcp.Manager // the MCP server; runs on the hub only
}

// New loads settings and prepares the components; nothing listens yet.
func New(opts Options) (*App, error) {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	cfg, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	if opts.PortOverride != 0 {
		cfg.ProxyPort = opts.PortOverride
		if err := cfg.Validate(); err != nil {
			return nil, err
		}
	}

	// Everything persistent sits beside the settings file.
	dataDir := filepath.Dir(opts.ConfigPath)

	store := capture.NewStore()
	ic, err := intercept.New(cfg.Intercept) // starts switched off
	if err != nil {
		return nil, err
	}
	// The CA for HTTPS interception is generated on first launch and kept in
	// the data directory.
	ca, err := tlsca.LoadOrCreate(filepath.Join(dataDir, "ca"))
	if err != nil {
		return nil, err
	}
	// Targets live with the open engagement; the data directory's own file
	// is what is used while none is open.
	targets, err := target.NewStore(filepath.Join(dataDir, "targets.json"))
	if err != nil {
		return nil, err
	}
	a := &App{
		log:        log,
		configPath: opts.ConfigPath,
		store:      store,
		intercept:  ic,
		secrets:    secrets.New(dataDir),
		instanceID: hub.NewInstanceID(),
		started:    time.Now(),
		agents:     agent.NewManager(filepath.Join(dataDir, "agents"), nil),
		broker:     approval.NewBroker(0),
		ca:         ca,
		targets:    targets,
		findings:   findings.NewStore(),
		entities:   entities.NewStore(),
		files:      files.NewStore(),
		cfg:        cfg,
	}
	// The Terminal tab's shells. They start in the open engagement's
	// folder, worked out as each one starts, so a terminal opened after an
	// engagement is where the work is.
	a.terminals = terminal.NewManager(terminal.Options{
		Env: a.terminalEnv(),
		Dir: func() string {
			if e, ok := a.engagements.Current(); ok {
				return e.Dir
			}
			return ""
		},
		Log: log,
	})
	// The finding types are the user's taxonomy, from the settings file;
	// Validate has already checked them, so this cannot fail.
	if err := a.findings.SetTypes(cfg.Findings.Types); err != nil {
		return nil, err
	}
	// Everything the store records is written to the open engagement's
	// archive. Subscribing here, before anything can be captured, means no
	// flow is ever missed; Record itself only queues (see internal/archive).
	a.recorder = archive.NewRecorder(nil)
	store.Subscribe(a.recorder.Record)
	// Likewise the Entity Explorer: Record only queues, and the indexer
	// parses each finished flow on its own goroutine.
	a.indexer = entities.NewIndexer(a.entities)
	store.Subscribe(a.indexer.Record)
	// Autocrawl records what it crawls in the same database, and follows
	// the targets: a host promoted while it runs is crawled in that run.
	a.newCrawler()
	targets.Subscribe(a.crawler.Rescope)
	// What the targets cover decides which hosts the Explorer draws and
	// which are only related to them, and each target's domain or address
	// is an entity of its own; a change of targets (a related host
	// promoted, a target added) is applied at once.
	a.followTargets()
	targets.Subscribe(a.followTargets)
	// The findings reviewer watches the findings store and has a background
	// agent session score each new finding. Subscribing here, before anything
	// is recorded, means none is missed; it stays idle until Start names the
	// reviewing profile (see configure), so nothing runs before the runtime.
	a.reviewer = newFindingsReviewer(a.agents, a.findings, log)
	a.findings.Subscribe(a.reviewer.onFinding)
	a.agents.Subscribe(a.reviewer.onAgent)

	a.engagements = engagement.NewManager(engagement.StatePath(opts.ConfigPath), targets.Path(), targets)
	a.engagements.Subscribe(a.followEngagement)
	// Reopen the engagement that was open last session, so the user picks up
	// where they left off, with the traffic collected under it. One that has
	// been moved or deleted is forgotten rather than fatal.
	if err := a.engagements.Restore(); err != nil {
		log.Warn("couldn't reopen the last engagement", "err", err)
	}
	a.followEngagement() // no engagement open: the data directory's own archive
	a.dns = dns.New(dns.Options{
		Store:     store,
		Intercept: ic,
		Scope:     a.dnsScope,
		Remote:    func() bool { return a.upstreams.Load().ResolvesRemotely() },
	})
	// A cached answer was made under the targets as they were; a change to
	// them (DNS interception switched on, say) should apply at once.
	targets.Subscribe(a.dns.Flush)
	// The upstream chain and the suppression list in force depend on the
	// open engagement's overrides as well as the user's settings (see
	// engagementsettings.go); this builds the dialer for the first time.
	a.followEngagementSettings()
	a.engagements.Subscribe(a.followEngagementSettings)
	// Replays go through the same chain as intercepted traffic, so a target
	// only reachable upstream can be replayed to as well.
	a.replays = replay.NewSessions(func(ctx context.Context, r replay.Request) (replay.Response, error) {
		r.Dial = a.dialResolved
		return replay.Send(ctx, r)
	})
	// Every replay, the user's or an agent's, is listed with the rest of
	// the traffic (and archived with it), marked with where it came from.
	a.replays.RecordTo(store)
	a.proxy = proxy.New(store,
		proxy.WithInterceptor(ic),
		proxy.WithCA(ca),
		proxy.WithLogger(log),
		proxy.WithScope(a.scope),
		proxy.WithDialer(a.dialUpstream),
		proxy.WithResolver(a.resolveForProxy),
		proxy.WithOrigin(a.pluginOrigin))
	a.newPlugins()
	a.proxy.SetWebSocket(wsHandler{a.plugins})
	// What interception holds for the user is configured per target.
	ic.SetUserHold(a.interceptHold)
	return a, nil
}

// dialUpstream opens a connection to addr through the configured chain of
// upstream proxies, or directly when there is none. It implements
// proxy.DialFunc.
func (a *App) dialUpstream(ctx context.Context, network, addr string) (net.Conn, error) {
	return a.upstreams.Load().DialContext(ctx, network, addr)
}

// dialResolved is dialUpstream with the host resolved through a.dns first,
// so the lookup is recorded as the dialling replay's or plugin's (see
// capture.WithSource) and can be intercepted.
func (a *App) dialResolved(ctx context.Context, network, addr string) (net.Conn, error) {
	return a.dns.Dialer(a.dialUpstream)(ctx, network, addr)
}

// resolveForProxy implements proxy.ResolveFunc through a.dns.
func (a *App) resolveForProxy(ctx context.Context, host, clientAddr string, source capture.Source, origin string) ([]string, error) {
	ans, err := a.dns.Resolve(ctx, host, dns.Who{Source: source, Origin: origin, ClientAddr: clientAddr})
	return ans.Hosts, err
}

// dnsScope implements dns.ScopeFunc: a lookup is recorded, and tagged, as
// an HTTP request to the same host would be, except that a target's path
// rules don't apply (a lookup has no path). Its answer's addresses can put
// it in an IP or CIDR target.
func (a *App) dnsScope(host string, ips []netip.Addr) (string, bool) {
	set := a.targets.Set()
	a.mu.Lock()
	required := a.requiredLocked()
	a.mu.Unlock()
	if set.Empty() {
		return "", true
	}
	m, ok := set.MatchHost(host, ips...)
	if !ok {
		return "", !required
	}
	return m.ID, true
}

// entityScope implements entities.ScopeFunc: the target whose address
// covers a host, path rules aside.
func (a *App) entityScope(host string) (string, bool) {
	m, ok := a.targets.Set().MatchHost(host)
	return m.ID, ok
}

// followTargets tells the Entity Explorer about the targets: which hosts
// they cover, and the domain or address each one names.
func (a *App) followTargets() {
	a.entities.SetScope(a.entityScope)
	if err := a.entities.SyncTargets(entitySeeds(a.targets.List())); err != nil {
		a.log.Warn("recording the targets' entities", "err", err)
	}
}

// entitySeeds is the entity each target stands for: the name host of a
// domain or IP target (which brings the name's registrable domain), or a
// wildcard's registrable domain. A regex or a CIDR range names no one
// entity.
func entitySeeds(targets []target.Target) []entities.Seed {
	var out []entities.Seed
	for _, t := range targets {
		name := strings.ToLower(strings.TrimSpace(t.Value))
		switch t.Kind {
		case target.KindWildcard:
			name = strings.TrimLeft(name, "*.")
			if strings.ContainsAny(name, "*?[") {
				continue
			}
		case target.KindDomain, target.KindIP:
		default:
			continue
		}
		name = strings.TrimSuffix(entities.BareHost(name), ".")
		switch {
		case name == "":
		case t.Kind != target.KindWildcard:
			// One name or address: its host (and, for a name, the domain
			// it sits under).
			out = append(out, entities.Seed{Type: entities.TypeHost, Name: name, TargetID: t.ID})
		case entities.RegistrableDomain(name) != "":
			out = append(out, entities.Seed{Type: entities.TypeDomain, Name: entities.RegistrableDomain(name), TargetID: t.ID})
		}
	}
	return out
}

// rebuildUpstream replaces the dialer with one for chain, reading each
// proxy's password from the secrets file. A proxy whose password can't be
// read is still used, without one: a chain that is half-applied would be
// harder to diagnose than an authentication failure.
func (a *App) rebuildUpstream(chain upstream.Chain) {
	hops := chain.Hops(func(id string) string {
		pw, err := a.secrets.ProxyPassword(id)
		if err != nil {
			a.log.Warn("reading upstream proxy password", "proxy", id, "err", err)
		}
		return pw
	})
	a.upstreams.Store(upstream.NewDialer(hops))
	// Whether names are resolved here or upstream may have changed.
	a.dns.Flush()
	if len(hops) > 0 {
		a.log.Info("upstream proxy chain active", "hops", len(hops))
	}
}

// scope implements proxy.ScopeFunc: it tags a request with the target it
// belongs to and, when a target is required, keeps everything else out of
// the capture (config.Targets.Required).
//
// This runs for every request, so it avoids locking: the compiled set comes
// from an atomic pointer, and the toggle from one guarded read.
func (a *App) scope(host string, ip netip.Addr, path string) (string, bool) {
	set := a.targets.Set()
	a.mu.Lock()
	required := a.requiredLocked()
	a.mu.Unlock()

	if set.Empty() {
		// Nothing configured yet. Requiring a target would silently collect
		// nothing, so the first run still records everything.
		return "", true
	}
	m, ok := set.Match(host, ip, path)
	if !ok {
		return "", !required
	}
	return m.ID, true
}

// interceptHold implements intercept.UserHoldFunc: when interception is on,
// a message is held if the target it falls under holds that direction. With
// no targets configured every request is held (so turning interception on
// does something before targets are set up).
func (a *App) interceptHold(dir intercept.Direction, s intercept.Subject) bool {
	set := a.targets.Set()
	if s.DNS {
		// Lookups are only held for a target that opts in to them, and
		// then as its requests/responses choice says.
		var ips []netip.Addr
		if s.IP.IsValid() {
			ips = append(ips, s.IP)
		}
		m, ok := set.MatchHost(s.Host, ips...)
		if !ok || !m.Intercept.DNS {
			return false
		}
		if dir == intercept.Response {
			return m.Intercept.Responses
		}
		return m.Intercept.Requests
	}
	if set.Empty() {
		return dir == intercept.Request
	}
	m, ok := set.Match(s.Host, s.IP, s.Path)
	if !ok {
		return false
	}
	if dir == intercept.Response {
		return m.Intercept.Responses
	}
	return m.Intercept.Requests
}

// Targets implements ui.Controller: the hosts in scope.
func (a *App) Targets() *target.Store { return a.targets }

// Engagements implements ui.Controller: the open engagement and the File
// menu's New/Open/Close.
func (a *App) Engagements() *engagement.Manager { return a.engagements }

// Findings implements ui.Controller: what the work turned up. The same store
// backs the Findings section, the create_finding tool and, later, plugins.
func (a *App) Findings() *findings.Store { return a.findings }

// Files implements ui.Controller: what the work recovered. The same store
// backs the Files section, the file tools, the command line and plugins'
// files.add, so every route records a recovered file the same way.
func (a *App) Files() *files.Store { return a.files }

// Entities implements ui.Controller: the engagement's Entity Explorer,
// which the Explorer section draws.
func (a *App) Entities() *entities.Store { return a.entities }

// DataDir implements ui.Controller: the directory holding every piece of
// persistent state, settings file included.
func (a *App) DataDir() string { return filepath.Dir(a.configPath) }

// Start brings the proxy up. A failure (typically the port being in use) is
// not fatal: it is reported through ProxyStatus so the user can pick another
// port from the UI.
func (a *App) Start() {
	a.mu.Lock()
	// If the configured port is taken (another reproxy, or another program),
	// step up one port at a time until one is free, so a second session
	// starts without the user having to pick a port. The found port is used
	// for this session only and isn't saved.
	bound, err := a.listenFromFreePort()
	a.proxyErr = err
	if err != nil {
		a.log.Error("proxy failed to start", "err", err)
	} else if bound != a.cfg.ProxyPort {
		a.log.Info("configured proxy port was busy; using the next free one",
			"configured", a.cfg.ProxyPort, "using", bound)
		a.cfg.ProxyPort = bound
	}
	a.mu.Unlock()
	a.startPlugins()

	// Join the other reproxy processes using this data directory (or lead
	// them). Failing to is not fatal: the app works alone, but agents
	// can't reach it from elsewhere.
	// The command line runs inside a process rather than beside one: this
	// instance answers `reproxy <namespace> <action>` against its own
	// stores, engagement and plugins (internal/cli). The commands act on
	// the same Local an agent reaches, so `reproxy flows ls` and an agent's
	// list_flows are the same code.
	local := a.localInstance()
	local.SetCommands(a.newCommandRegistry())
	a.mu.Lock()
	a.local = local
	a.mu.Unlock()

	node, err := hub.Start(hub.Options{
		DataDir:      filepath.Dir(a.configPath),
		Local:        local,
		Token:        a.secrets.MCPToken,
		Logger:       a.log,
		OnRoleChange: a.onHubRole,
	})
	if err != nil {
		a.log.Error("couldn't join other reproxy processes", "err", err)
		return
	}

	// The MCP server serves the tool registry to external agents over the
	// group. It runs on the hub, whose Router reaches every instance; other
	// processes drive it through the hub/mcp route below.
	reg := mcp.New(node.Router(), a.broker, a.findings, a.entities)
	reg.SetSourceMapExporter(a.ExportSourceMap)
	reg.SetFiles(a.files)
	reg.SetPluginActions(a.pluginActions, a.runPluginAction)
	mgr := mcp.NewManager(reg, mcp.Options{
		Token:  a.secrets.MCPToken,
		Policy: func() string { return a.Settings().MCP.ExternalPolicy },
		Logger: a.log,
	})
	node.HandleHub(hub.HubPrefix+"mcp/{action}", mgr.ControlHandler(func() string { return a.Settings().MCP.Addr() }))

	// The command line reaches the group through the hub, which hands the
	// request to the instance --instance names, or to its own.
	node.HandleHub("POST "+hub.HubPrefix+cli.HubPath, cli.HubHandler(a.resolveInstance))

	// The in-app agents drive the same tool registry as external clients.
	runner := agent.NewLoopRunner(agentProviders{a}, agentTools{reg})
	a.agents.SetRunner(runner)
	// Now the runtime exists, the findings reviewer can run: name the agent
	// profile designated to review new findings (the first with the toggle on).
	a.reviewer.configure(a.AgentConfig())

	a.mu.Lock()
	a.node = node
	a.mcp = mgr
	cfg := a.cfg
	a.mu.Unlock()

	// Creating an agent session starts the MCP server (so its state shows in
	// the footer and external clients can attach), unless it is already on.
	a.agents.Subscribe(func(e agent.Event) {
		if e.Kind == agent.SessionAdded {
			a.ensureMCP()
		}
	})

	// The MCP server always runs while reproxy is open. It is shared across
	// sessions: only the hub process binds the port, and the others reach it
	// through the hub, so a second session detects the running server rather
	// than starting its own. (onHubRole fires during hub.Start, before the
	// manager exists, so the initial start happens here.)
	//
	// Future: once reproxy ships as an installed package, the server will run
	// as a background service that the app starts and stops here instead of
	// in-process.
	if node.IsHub() {
		if err := mgr.Start(cfg.MCP.Addr()); err != nil {
			a.log.Error("MCP server failed to start", "err", err)
		}
	}
}

// listenFromFreePort starts the proxy on the configured port, or the next
// free port above it when that one is in use. It returns the port actually
// bound. A non-"address in use" failure (e.g. a privileged port, or a host
// that isn't ours) is returned as-is, described for the user.
func (a *App) listenFromFreePort() (int, error) {
	const maxTries = 64
	cfg := a.cfg
	for i := 0; i < maxTries; i++ {
		port := cfg.ProxyPort + i
		if port > 65535 {
			break
		}
		try := cfg
		try.ProxyPort = port
		err := a.proxy.Listen(try.ProxyAddr())
		if err == nil {
			return port, nil
		}
		if !errors.Is(err, syscall.EADDRINUSE) {
			return 0, describeListenError(try, err)
		}
	}
	return 0, describeListenError(cfg, syscall.EADDRINUSE)
}

// onHubRole starts the MCP server when this process becomes the hub (if the
// user wants it on launch) and stops it when it stops being the hub, so the
// server follows the hub role across processes. It runs on the Node's
// goroutine, so it must not block for long.
func (a *App) onHubRole(isHub bool) {
	a.mu.Lock()
	mgr := a.mcp
	addr := a.cfg.MCP.Addr()
	a.mu.Unlock()
	if mgr == nil {
		return // not wired yet; Start handles the initial role
	}
	if !isHub {
		mgr.Stop() // the new hub runs it now
		return
	}
	if err := mgr.Start(addr); err != nil {
		a.log.Error("MCP server failed to start", "err", err)
	}
}

// MCPStatus reports whether the MCP server is listening (stopped before the
// group is joined).
func (a *App) MCPStatus() mcp.Status {
	a.mu.Lock()
	mgr, node := a.mcp, a.node
	a.mu.Unlock()
	if mgr == nil || node == nil {
		return mcp.Status{}
	}
	if node.IsHub() {
		return mgr.Status()
	}
	// The server runs on the hub; ask it (fall back to local, i.e. stopped).
	if st, ok := a.hubMCPStatus(); ok {
		return st
	}
	return mgr.Status()
}

// StartMCP starts the MCP server (on the hub) at the configured address, or
// asks the hub to if this process isn't it.
func (a *App) StartMCP() error {
	a.mu.Lock()
	mgr, node := a.mcp, a.node
	addr := a.cfg.MCP.Addr()
	a.mu.Unlock()
	if mgr == nil || node == nil {
		return errNotJoined
	}
	if node.IsHub() {
		return mgr.Start(addr)
	}
	return a.mcpControl("start")
}

// StopMCP stops the MCP server.
func (a *App) StopMCP() error {
	a.mu.Lock()
	mgr, node := a.mcp, a.node
	a.mu.Unlock()
	if mgr == nil || node == nil {
		return errNotJoined
	}
	if node.IsHub() {
		return mgr.Stop()
	}
	return a.mcpControl("stop")
}

// ensureMCP starts the server if it isn't already running, best-effort.
func (a *App) ensureMCP() {
	if st := a.MCPStatus(); st.Running {
		return
	}
	if err := a.StartMCP(); err != nil {
		a.log.Warn("couldn't auto-start MCP server", "err", err)
	}
}

// Node returns this process's membership of the reproxy group, or nil if
// joining failed.
func (a *App) Node() *hub.Node {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.node
}

// localInstance is this process as an agent-facing instance.
func (a *App) localInstance() *instance.Local {
	return instance.NewLocal(
		instance.Info{ID: a.instanceID, PID: os.Getpid(), Started: a.started},
		a.store, a.intercept, a.replays, &a.focus, a.ProxyStatus)
}

// Close shuts down launched browsers (useless without the proxy) and then
// the proxy.
func (a *App) Close() error {
	a.mu.Lock()
	sessions := a.browsers
	a.browsers = nil
	node := a.node
	a.node = nil
	mgr := a.mcp
	a.mcp = nil
	a.mu.Unlock()
	for _, s := range sessions {
		s.Stop()
	}
	// Plugins stop before what they act on.
	a.pluginMu.Lock()
	a.plugins.Close()
	a.pluginMu.Unlock()
	if mgr != nil {
		mgr.Stop()
	}
	if node != nil {
		node.Close()
	}
	// The shells go with the window that showed them.
	a.terminals.CloseAll()
	// The findings reviewer stops before the stores it watches close.
	a.reviewer.stop()
	// The crawl stops before the archive it records into closes.
	a.crawler.Stop()
	a.crawler.Wait()
	err := a.proxy.Close()
	// After the proxy, so a request still in flight reaches the archive.
	if aerr := a.closeArchive(); err == nil {
		err = aerr
	}
	return err
}

// InstalledBrowsers implements ui.Controller.
func (a *App) InstalledBrowsers() []browser.Browser { return browser.Detect() }

// CACertPEM implements ui.Controller: the proxy's CA certificate for download.
func (a *App) CACertPEM() []byte { return a.ca.CertPEM() }

// CAFingerprint implements ui.Controller.
func (a *App) CAFingerprint() string { return a.ca.Fingerprint() }

// LaunchBrowser implements ui.Controller: it starts the configured browser
// (or the first one installed) with its own profile under the data
// directory, pointed at the proxy.
func (a *App) LaunchBrowser() (*browser.Session, error) {
	a.mu.Lock()
	cfg, proxyErr := a.cfg, a.proxyErr
	a.mu.Unlock()
	if proxyErr != nil {
		return nil, fmt.Errorf("the proxy isn't running: %w", proxyErr)
	}

	var b browser.Browser
	if cfg.Browser.Path != "" {
		var err error
		if b, err = browser.FromPath(cfg.Browser.Path); err != nil {
			return nil, err
		}
	} else {
		found := browser.Detect()
		if len(found) == 0 {
			return nil, browser.ErrNoBrowser
		}
		b = found[0]
	}

	s, err := browser.Launch(b, browser.Options{
		ProxyAddr:   a.proxy.Addr(),
		ProfileRoot: filepath.Join(filepath.Dir(a.configPath), "browser"),
		TrustedCA:   a.ca.CertPEM(),
	})
	if err != nil {
		return nil, err
	}
	a.log.Info("browser launched", "browser", b.Name, "path", b.Path)

	a.mu.Lock()
	// Forget sessions that have already exited.
	live := a.browsers[:0]
	for _, old := range a.browsers {
		select {
		case <-old.Done():
		default:
			live = append(live, old)
		}
	}
	a.browsers = append(live, s)
	a.mu.Unlock()
	return s, nil
}

// Store implements ui.Controller.
func (a *App) Store() *capture.Store { return a.store }

// UIState implements ui.Controller. An unreadable layout file is logged
// and the default layout used; it's never worth refusing to start over.
func (a *App) UIState() config.UIState {
	s, err := config.LoadUIState(config.UIStatePath(a.configPath))
	if err != nil {
		a.log.Warn("ignoring saved window layout", "err", err)
		return config.UIState{}
	}
	return s
}

// SaveUIState implements ui.Controller.
func (a *App) SaveUIState(s config.UIState) error {
	return config.SaveUIState(config.UIStatePath(a.configPath), s)
}

// ReplaySessions implements ui.Controller.
func (a *App) ReplaySessions() *replay.Sessions { return a.replays }

// Focus implements ui.Controller.
func (a *App) Focus() *focus.Tracker { return &a.focus }

// Agents implements ui.Controller.
func (a *App) Agents() *agent.Manager { return a.agents }

// Terminals implements ui.Controller: the shells behind the Terminal tab.
func (a *App) Terminals() *terminal.Manager { return a.terminals }

// Approvals implements ui.Controller.
func (a *App) Approvals() *approval.Broker { return a.broker }

// AgentConfig implements ui.Controller. An unreadable agents.json is
// logged and treated as empty rather than refusing to start.
func (a *App) AgentConfig() config.Agents {
	ag, err := config.LoadAgents(config.AgentsPath(a.configPath))
	if err != nil {
		a.log.Warn("ignoring agent configuration", "err", err)
		return config.Agents{}
	}
	return ag
}

// SaveAgentConfig implements ui.Controller.
func (a *App) SaveAgentConfig(ag config.Agents, keys map[string]string) error {
	if err := ag.Validate(); err != nil {
		return err
	}
	old := a.AgentConfig()
	if err := config.SaveAgents(config.AgentsPath(a.configPath), ag); err != nil {
		return err
	}
	for _, p := range old.Profiles {
		if _, ok := ag.Profile(p.ID); !ok {
			if err := a.secrets.SetAPIKey(p.ID, ""); err != nil {
				return err
			}
		}
	}
	for _, p := range ag.Profiles {
		key, ok := keys[p.ID]
		if !ok || key == a.APIKey(p.ID) {
			continue
		}
		if err := a.secrets.SetAPIKey(p.ID, key); err != nil {
			return err
		}
	}
	// A profile may have been added, removed or had its review toggle changed,
	// which decides who reviews new findings.
	if a.reviewer != nil {
		a.reviewer.configure(ag)
	}
	return nil
}

// APIKey implements ui.Controller.
func (a *App) APIKey(profileID string) string {
	k, err := a.secrets.APIKey(profileID)
	if err != nil {
		a.log.Warn("reading API key", "err", err)
	}
	return k
}

// UpstreamPassword implements ui.Controller.
func (a *App) UpstreamPassword(proxyID string) string {
	pw, err := a.secrets.ProxyPassword(proxyID)
	if err != nil {
		a.log.Warn("reading upstream proxy password", "err", err)
	}
	return pw
}

// SaveUpstreamPasswords implements ui.Controller. The chain itself is saved
// with the rest of the settings (or the engagement's); only the passwords
// go here, so they stay out of config.json and out of the engagement
// folder. A "" removes a proxy's password; proxies not mentioned keep
// theirs. The dialer picks them up on the apply that follows.
func (a *App) SaveUpstreamPasswords(byID map[string]string) error {
	return a.secrets.UpdateProxyPasswords(byID)
}

// MCPToken implements ui.Controller.
func (a *App) MCPToken() (string, error) { return a.secrets.MCPToken() }

// RegenerateMCPToken implements ui.Controller.
func (a *App) RegenerateMCPToken() (string, error) { return a.secrets.RegenerateMCPToken() }

// Interceptor implements ui.Controller.
func (a *App) Interceptor() *intercept.Interceptor { return a.intercept }

// Settings implements ui.Controller.
func (a *App) Settings() config.Config {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

// ProxyStatus implements ui.Controller.
func (a *App) ProxyStatus() (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.proxyErr != nil {
		return "", a.proxyErr
	}
	return a.proxy.Addr(), nil
}

// ApplySettings implements ui.Controller. The proxy is moved first; settings
// are only saved once the new port is actually listening.
func (a *App) ApplySettings(next config.Config) error {
	if err := next.Validate(); err != nil {
		return err
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if err := next.Effective(a.eng).Validate(); err != nil {
		return fmt.Errorf("with the open engagement's own settings: %w", err)
	}

	if next.ProxyAddr() != a.cfg.ProxyAddr() || a.proxyErr != nil {
		if err := a.proxy.Listen(next.ProxyAddr()); err != nil {
			return describeListenError(next, err)
		}
		a.proxyErr = nil
	}
	if err := a.intercept.Configure(next.Intercept); err != nil {
		return err // already validated, so not expected
	}
	a.cfg = next
	if err := a.findings.SetTypes(next.Findings.Types); err != nil {
		return err // already validated, so not expected
	}
	// The upstream chain and suppressions in force are the new settings
	// combined with the open engagement's overrides.
	a.applyEffectiveLocked()

	if err := config.Save(a.configPath, next); err != nil {
		return fmt.Errorf("settings applied but not saved: %w", err)
	}
	// Plugins' lifecycle hooks run without the app's lock held.
	go a.applyPlugins()
	return nil
}

// describeListenError turns the common bind failures into a message a user
// can act on. Unrecognised errors are returned unchanged.
func describeListenError(c config.Config, err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, syscall.EADDRINUSE):
		return fmt.Errorf("port %d is already in use by another program", c.ProxyPort)
	case errors.Is(err, syscall.EACCES):
		return fmt.Errorf("not allowed to listen on port %d (ports below 1024 usually need admin rights)", c.ProxyPort)
	case errors.Is(err, syscall.EADDRNOTAVAIL):
		return fmt.Errorf("%s is not an address of this machine", c.ProxyHost)
	}
	return err
}
