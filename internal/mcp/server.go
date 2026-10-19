package mcp

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Path is where the MCP endpoint is served.
const Path = "/mcp"

// Options configure a server's HTTP layer.
type Options struct {
	// Token returns the bearer token every request must carry.
	Token func() (string, error)
	// Policy returns the approval policy for external clients
	// (config.MCP.ExternalPolicy). Read once per call so a settings change
	// takes effect without a restart.
	Policy func() string
	Logger *slog.Logger
}

// Handler builds the HTTP handler that serves the registry's tools to
// external MCP clients at Path, behind bearer-token authentication. It is
// transport only: no listener is opened.
func Handler(reg *Registry, opts Options) http.Handler {
	// Normalise once, here, so every server buildServer makes below gets a
	// logger: the SDK is handed opts.Logger directly, and a nil one would
	// mean the transport's errors go nowhere. NewManager does the same for
	// its own logging.
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}

	stream := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server {
		return buildServer(reg, opts)
	}, &sdk.StreamableHTTPOptions{Stateless: true})

	authed := authMiddleware(opts.Token, stream)
	mux := http.NewServeMux()
	mux.Handle(Path, authed)
	mux.Handle(Path+"/", authed)
	return mux
}

// buildServer makes a fresh SDK server exposing every tool. In stateless
// mode a server is built per request, so this stays cheap.
func buildServer(reg *Registry, opts Options) *sdk.Server {
	srv := sdk.NewServer(&sdk.Implementation{Name: "reproxy", Version: "0.1"}, &sdk.ServerOptions{
		Instructions: "Tools to inspect and drive reproxy's intercepting proxy: read captured flows, control interception, and edit and replay requests. Requests you send are confined to the domain each flow originally targeted.",
		Logger:       opts.Logger,
	})
	for _, t := range reg.Tools() {
		srv.AddTool(&sdk.Tool{
			Name:        t.Name,
			Description: t.Description,
			InputSchema: t.Schema,
		}, adapt(reg, t, opts.Policy))
	}
	return srv
}

// adapt wraps a registry tool as an SDK tool handler for external clients.
func adapt(reg *Registry, t *Tool, policy func() string) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		c := Caller{Policy: externalPolicy(policy)}
		if req.Session != nil {
			if ip := req.Session.InitializeParams(); ip != nil && ip.ClientInfo != nil {
				c.Client = ip.ClientInfo.Name
			}
		}
		out, err := reg.Call(ctx, c, t.Name, rawArgs(req))
		if err != nil {
			// Report tool errors in the result (IsError) so the model sees
			// them and can adjust, rather than as a protocol-level failure.
			return &sdk.CallToolResult{
				IsError: true,
				Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}},
			}, nil
		}
		text, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return nil, err
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(text)}}}, nil
	}
}

func rawArgs(req *sdk.CallToolRequest) json.RawMessage {
	if req.Params == nil {
		return nil
	}
	return req.Params.Arguments
}

func externalPolicy(policy func() string) string {
	if policy == nil {
		return ""
	}
	return policy()
}

// authMiddleware rejects requests that don't carry the shared bearer token.
func authMiddleware(token func() (string, error), next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		want, err := token()
		if err != nil || want == "" {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || subtle.ConstantTimeCompare([]byte(got), []byte(want)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// Status reports whether the MCP server is listening.
type Status struct {
	Running bool   `json:"running"`
	Addr    string `json:"addr,omitempty"`
	Error   string `json:"error,omitempty"`
}

// Manager owns the MCP server's listener. It runs on the hub process only;
// Start, Stop and Status are safe to call from any goroutine.
type Manager struct {
	reg  *Registry
	opts Options
	log  *slog.Logger

	mu      sync.Mutex
	srv     *http.Server
	ln      net.Listener
	addr    string
	running bool
	lastErr error
}

// NewManager returns a stopped Manager serving reg.
func NewManager(reg *Registry, opts Options) *Manager {
	log := opts.Logger
	if log == nil {
		log = slog.Default()
	}
	return &Manager{reg: reg, opts: opts, log: log}
}

// Start listens on addr and serves the tools. Starting on the address it is
// already serving is a no-op; starting on a different one moves the listener.
func (m *Manager) Start(addr string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.running {
		if m.addr == addr {
			return nil
		}
		m.stopLocked()
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		m.lastErr = err
		m.log.Error("MCP server failed to start", "addr", addr, "err", err)
		return err
	}
	srv := &http.Server{Handler: Handler(m.reg, m.opts), ReadHeaderTimeout: 10 * time.Second}
	m.srv, m.ln, m.addr, m.running, m.lastErr = srv, ln, ln.Addr().String(), true, nil
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			m.mu.Lock()
			m.lastErr = err
			m.mu.Unlock()
			m.log.Error("MCP server stopped", "err", err)
		}
	}()
	m.log.Info("MCP server listening", "addr", m.addr)
	return nil
}

// Stop stops serving. Stopping an already-stopped Manager is a no-op.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stopLocked()
}

func (m *Manager) stopLocked() error {
	if !m.running {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := m.srv.Shutdown(ctx)
	m.srv, m.ln, m.addr, m.running = nil, nil, "", false
	return err
}

// Status reports the current state.
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := Status{Running: m.running, Addr: m.addr}
	if m.lastErr != nil {
		st.Error = m.lastErr.Error()
	}
	return st
}

// ControlHandler serves start, stop and status over HTTP so a non-hub
// process can drive the hub's MCP server. Mount it (already authenticated)
// under the hub's route prefix:
//
//	node.HandleHub("POST "+hub.HubPrefix+"mcp/{action}", m.ControlHandler(addrFn))
//	node.HandleHub("GET "+hub.HubPrefix+"mcp/status", m.ControlHandler(addrFn))
//
// addrFn supplies the listen address for a start request.
func (m *Manager) ControlHandler(addrFn func() string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		action := r.PathValue("action")
		if action == "" { // mounted without an {action} wildcard
			action = r.URL.Path[strings.LastIndexByte(r.URL.Path, '/')+1:]
		}
		switch action {
		case "start":
			if err := m.Start(addrFn()); err != nil {
				writeJSON(w, http.StatusOK, map[string]any{"error": err.Error(), "status": m.Status()})
				return
			}
		case "stop":
			m.Stop()
		case "status":
			// fall through to report status
		default:
			http.Error(w, fmt.Sprintf("unknown action %q", action), http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, m.Status())
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v)
}
