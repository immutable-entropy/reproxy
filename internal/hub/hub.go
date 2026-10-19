// Package hub connects the reproxy processes running on one machine (for
// one data directory) so a single MCP server and a single set of agent
// sessions can work across all of them.
//
// Every process runs a Node. Each Node serves its own instance.Instance on
// an ephemeral loopback port. Exactly one Node, the hub, holds an OS lock on
// <datadir>/hub.lock; it writes hub.json (its ID and address), and the other
// Nodes register with it and keep heartbeating. The hub's Router reaches
// every instance: its own directly, the others over HTTP. When the hub
// process exits, the OS releases the lock, another Node takes it on its next
// poll, and the remaining Nodes re-register with the new hub.
//
// The hub's address also serves the routes added with HandleHub (the agent
// session API, for other processes' UIs). All endpoints require the shared
// bearer token from the secrets file.
package hub

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"reproxy/internal/atomicfile"
	"reproxy/internal/instance"
	"reproxy/internal/randid"
)

const (
	lockName = "hub.lock"
	infoName = "hub.json"
	// HubPrefix is the path prefix of hub-only routes.
	HubPrefix = "/hub/"
)

// Options configure a Node.
type Options struct {
	// DataDir is where hub.lock and hub.json live; processes sharing it
	// form one group.
	DataDir string
	// Local is this process's instance.
	Local *instance.Local
	// Token returns the shared bearer token (secrets.Store.MCPToken).
	Token  func() (string, error)
	Logger *slog.Logger
	// OnRoleChange is called (on the Node's own goroutine) when this
	// process becomes the hub. It must not block for long.
	OnRoleChange func(isHub bool)

	// Timing; zero values use the defaults. Tests shorten them.
	Poll      time.Duration // how often to try for the lock and heartbeat (1s)
	PeerTTL   time.Duration // forget a peer not heard from for this long (6s)
	CallLimit time.Duration // timeout for registration calls (2s)
}

// hubInfo is hub.json.
type hubInfo struct {
	ID   string `json:"id"`
	PID  int    `json:"pid"`
	Addr string `json:"addr"`
}

// Node is this process's membership of the group.
type Node struct {
	opts   Options
	log    *slog.Logger
	ln     net.Listener
	srv    *http.Server
	router *Router

	hubMux *http.ServeMux // routes served only while hub

	mu       sync.Mutex
	isHub    bool
	unlock   func()
	hubAddr  string // the current hub's address ("" if unknown)
	hubID    string
	lastBeat time.Time

	stop chan struct{}
	done chan struct{}
}

// NewInstanceID returns a random instance ID: short, so handles built from
// it stay readable for LLMs.
func NewInstanceID() string { return randid.Hex(4) }

// Start begins serving the local instance and joins (or becomes) the hub.
func Start(opts Options) (*Node, error) {
	if opts.Poll == 0 {
		opts.Poll = time.Second
	}
	if opts.PeerTTL == 0 {
		opts.PeerTTL = 6 * opts.Poll
	}
	if opts.CallLimit == 0 {
		opts.CallLimit = 2 * time.Second
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if err := os.MkdirAll(opts.DataDir, 0o755); err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, fmt.Errorf("hub: listening: %w", err)
	}

	n := &Node{
		opts:   opts,
		log:    opts.Logger,
		ln:     ln,
		router: newRouter(opts.Local, opts.Token),
		hubMux: http.NewServeMux(),
		stop:   make(chan struct{}),
		done:   make(chan struct{}),
	}
	n.hubMux.HandleFunc("POST "+HubPrefix+"register", n.handleRegister)

	mux := http.NewServeMux()
	mux.Handle(instance.PathPrefix, instance.Handler(opts.Local, n.authorized))
	mux.HandleFunc(HubPrefix, n.serveHub)
	n.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go n.srv.Serve(ln)

	n.tick() // settle the role before returning
	go n.loop()
	return n, nil
}

// Addr returns where this Node serves.
func (n *Node) Addr() string { return n.ln.Addr().String() }

// ID returns this instance's ID.
func (n *Node) ID() string { return n.opts.Local.ID() }

// IsHub reports whether this process is currently the hub.
func (n *Node) IsHub() bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.isHub
}

// Hub returns the current hub's address and ID, as last seen; the address
// is this Node's own when it is the hub.
func (n *Node) Hub() (addr, id string, ok bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	return n.hubAddr, n.hubID, n.hubAddr != ""
}

// Router reaches every instance in the group. Peers are only known while
// this Node is the hub; otherwise it holds just the local instance.
func (n *Node) Router() *Router { return n.router }

// HandleHub adds a route served on this Node's address while it is the hub.
// pattern is an http.ServeMux pattern under HubPrefix.
func (n *Node) HandleHub(pattern string, h http.Handler) { n.hubMux.Handle(pattern, h) }

// Authorized reports whether r carries the shared bearer token. Handlers
// added with HandleHub are already checked.
func (n *Node) Authorized(r *http.Request) bool { return n.authorized(r) }

// Token returns the shared bearer token, for calls to the hub.
func (n *Node) Token() (string, error) { return n.opts.Token() }

// Close leaves the group: it stops serving and, if hub, releases the lock
// so another process takes over.
func (n *Node) Close() error {
	select {
	case <-n.stop:
		return nil
	default:
	}
	close(n.stop)
	<-n.done
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := n.srv.Shutdown(ctx)

	n.mu.Lock()
	unlock := n.unlock
	n.unlock, n.isHub = nil, false
	n.mu.Unlock()
	if unlock != nil {
		// Remove hub.json only while still holding the lock, so it can't
		// delete a successor's.
		os.Remove(filepath.Join(n.opts.DataDir, infoName))
		unlock()
	}
	return err
}

func (n *Node) loop() {
	defer close(n.done)
	t := time.NewTicker(n.opts.Poll)
	defer t.Stop()
	for {
		select {
		case <-n.stop:
			return
		case <-t.C:
			n.tick()
		}
	}
}

// tick does one round: hubs prune silent peers; others try for the lock,
// and failing that make sure they are registered with the current hub.
func (n *Node) tick() {
	if n.IsHub() {
		n.router.prune(n.opts.PeerTTL)
		return
	}
	unlock, ok, err := tryLock(filepath.Join(n.opts.DataDir, lockName))
	if err != nil {
		n.log.Warn("hub: lock", "err", err)
	}
	if ok {
		n.becomeHub(unlock)
		return
	}
	n.followHub()
}

func (n *Node) becomeHub(unlock func()) {
	info := hubInfo{ID: n.ID(), PID: os.Getpid(), Addr: n.Addr()}
	if err := writeJSON(filepath.Join(n.opts.DataDir, infoName), info); err != nil {
		n.log.Error("hub: writing hub.json", "err", err)
	}
	n.mu.Lock()
	n.isHub, n.unlock = true, unlock
	n.hubAddr, n.hubID = info.Addr, info.ID
	n.mu.Unlock()
	n.log.Info("this process is now the reproxy hub", "id", info.ID, "addr", info.Addr)
	if n.opts.OnRoleChange != nil {
		n.opts.OnRoleChange(true)
	}
}

// followHub registers with the hub named in hub.json when it is new or a
// heartbeat is due.
func (n *Node) followHub() {
	var info hubInfo
	data, err := os.ReadFile(filepath.Join(n.opts.DataDir, infoName))
	if err == nil {
		err = json.Unmarshal(data, &info)
	}
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			n.log.Debug("hub: reading hub.json", "err", err)
		}
		return // the new hub hasn't written it yet
	}

	n.mu.Lock()
	changed := info.ID != n.hubID || info.Addr != n.hubAddr
	due := changed || time.Since(n.lastBeat) >= n.opts.Poll*2
	n.mu.Unlock()
	if !due {
		return
	}

	self, _ := n.opts.Local.Info(context.Background())
	err = n.post(info.Addr, HubPrefix+"register", registration{Info: self, Addr: n.Addr()})
	if err != nil {
		n.log.Debug("hub: registering", "hub", info.Addr, "err", err)
		return
	}
	n.mu.Lock()
	n.hubAddr, n.hubID, n.lastBeat = info.Addr, info.ID, time.Now()
	n.mu.Unlock()
	if changed {
		n.log.Info("joined reproxy hub", "hub", info.ID, "addr", info.Addr)
	}
}

type registration struct {
	Info instance.Info `json:"info"`
	Addr string        `json:"addr"`
}

func (n *Node) handleRegister(w http.ResponseWriter, r *http.Request) {
	var reg registration
	if err := json.NewDecoder(r.Body).Decode(&reg); err != nil || reg.Info.ID == "" || reg.Addr == "" {
		http.Error(w, "bad registration", http.StatusBadRequest)
		return
	}
	n.router.register(reg.Info, reg.Addr)
	w.WriteHeader(http.StatusNoContent)
}

func (n *Node) serveHub(w http.ResponseWriter, r *http.Request) {
	if !n.authorized(r) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	if !n.IsHub() {
		http.Error(w, "not the hub", http.StatusMisdirectedRequest)
		return
	}
	n.hubMux.ServeHTTP(w, r)
}

func (n *Node) authorized(r *http.Request) bool {
	want, err := n.opts.Token()
	if err != nil || want == "" {
		return false
	}
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

func (n *Node) post(addr, path string, body any) error {
	data, err := json.Marshal(body)
	if err != nil {
		return err
	}
	tok, err := n.opts.Token()
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), n.opts.CallLimit)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+path, strings.NewReader(string(data)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("%s", resp.Status)
	}
	return nil
}

// writeJSON writes the instance-info file every running process publishes
// for the hub to read. It names the loopback port and bearer token an
// agent connects with, so it is the user's alone.
func writeJSON(path string, v any) error {
	return atomicfile.WriteJSON(path, v, atomicfile.Private, 0o755)
}

// Location is where the hub is, as hub.json records it.
type Location struct {
	ID   string
	PID  int
	Addr string
}

// Locate reads hub.json, for a client that is not part of the group — the
// command line, which has to find the running reproxy rather than be one.
// fs.ErrNotExist means no process is running (or none has taken the role
// yet); the caller says so in its own words.
func Locate(dataDir string) (Location, error) {
	data, err := os.ReadFile(filepath.Join(dataDir, infoName))
	if err != nil {
		return Location{}, err
	}
	var info hubInfo
	if err := json.Unmarshal(data, &info); err != nil {
		return Location{}, fmt.Errorf("hub: %s: %w", infoName, err)
	}
	if info.Addr == "" {
		return Location{}, fmt.Errorf("hub: %s names no address", infoName)
	}
	return Location{ID: info.ID, PID: info.PID, Addr: info.Addr}, nil
}
