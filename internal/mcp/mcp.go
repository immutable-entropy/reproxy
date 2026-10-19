// Package mcp exposes reproxy's per-process operations — reading captured
// traffic, driving interception, working in Replay sessions and recording
// findings — as a single list of tools. The same list drives two callers:
// the MCP server that external agents connect to (see server.go) and, later,
// the in-app agent sessions. Keeping one registry means a tool is defined,
// gated and scope-checked in exactly one place.
//
// A tool resolves the instance it works on through a Router (the hub's, in
// production) and never talks to Qt. Tools that send or change traffic set
// Mutates: they pass the approval gate and enforce the scope rule from
// CLAUDE.md before doing anything. Tools that write into the engagement's
// own record without touching traffic — recording a finding — set Writes
// instead: a read-only client may not call them, but they don't interrupt
// the user for approval.
package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"reproxy/internal/approval"
	"reproxy/internal/capture"
	"reproxy/internal/config"
	"reproxy/internal/entities"
	"reproxy/internal/files"
	"reproxy/internal/findings"
	"reproxy/internal/instance"
	"reproxy/internal/replay"
)

// Router finds instances by ID. It is the subset of *hub.Router the tools
// need, so the registry can be tested without a running hub.
type Router interface {
	// LocalID is this process's instance ID, used when a tool is called
	// without naming an instance.
	LocalID() string
	// Get returns the instance with the given ID.
	Get(id string) (instance.Instance, bool)
	// Instances describes every reachable instance.
	Instances(ctx context.Context) []instance.Info
}

// Caller is who is invoking a tool and under what approval policy.
type Caller struct {
	// Session is the in-app agent session asking, or "" for an external
	// MCP client.
	Session string
	// Client names an external MCP client, when it told us its name.
	Client string
	// Policy is one of config.PolicyAsk, config.PolicyAuto or
	// config.PolicyReadOnly. An unknown value is treated as ask.
	Policy string
	// AskedAt is when the user sent the chat message being answered (in-app
	// sessions only), so "this request" resolves to what was focused then
	// rather than to wherever the user has clicked since.
	AskedAt time.Time
}

// owner is who a Replay session or send is attributed to.
func (c Caller) owner() replay.Owner {
	switch {
	case c.Session != "":
		return replay.AgentOwner(c.Session)
	case c.Client != "":
		return replay.Owner("mcp:" + c.Client)
	default:
		return replay.Owner("mcp")
	}
}

// who identifies the caller for log and error text.
func (c Caller) who() string {
	switch {
	case c.Session != "":
		return "agent session " + c.Session
	case c.Client != "":
		return c.Client
	default:
		return "an MCP client"
	}
}

// Errors a tool returns for the model to read.
var (
	// ErrReadOnly is returned when a read-only caller invokes a tool that
	// would send or change traffic, or write into the engagement.
	ErrReadOnly = errors.New("this client is read-only and may not send or change traffic, or record findings")
	// ErrNoFindings is returned when a findings tool is called in a process
	// that has no findings store wired up.
	ErrNoFindings = errors.New("this instance has no findings store")
	ErrNoEntities = errors.New("this instance has no Entity Explorer")
	// ErrNoFiles is returned when a file tool is called in a process with no
	// recovered-file store wired up.
	ErrNoFiles = errors.New("this instance has no record of recovered files")
	// ErrDenied is returned when the user denies an approval request.
	ErrDenied = errors.New("the user denied this request")
)

// Tool is one operation in the registry.
type Tool struct {
	// Name is the MCP tool name.
	Name string
	// Description tells the model what the tool does.
	Description string
	// Mutates is true for tools that send or change traffic: they pass the
	// approval gate and enforce the scope rule.
	Mutates bool
	// Writes is true for tools that change what the engagement has recorded
	// without touching traffic. A read-only caller is refused; everyone
	// else runs without an approval prompt, because nothing leaves the
	// machine — the Findings badge is the notification.
	Writes bool
	// Schema is the JSON Schema of the tool's arguments.
	Schema map[string]any

	run func(ctx context.Context, r *Registry, c Caller, args json.RawMessage) (any, error)
}

// FindingStore is what the findings tools need from this process's findings
// (see internal/findings). Findings belong to the open engagement, which is
// a property of one process, so they are not routed through an Instance the
// way flows and Replay sessions are: the tools act on the local store.
type FindingStore interface {
	// Types is the catalog a finding can be classified with.
	Types() []findings.Type
	// Record adds a finding, or merges the draft into the one that already
	// holds its value; created is false for a merge.
	Record(findings.Draft) (f findings.Finding, created bool, err error)
	// Select returns the findings matching a query, newest first.
	Select(findings.Query) []findings.Finding
	// Get returns one finding by ID.
	Get(id uint64) (findings.Finding, bool)
	// SetConfidence scores a finding 0–100, or marks it unreviewed with a nil
	// score (see findings.Finding.Confidence).
	SetConfidence(id uint64, confidence *int) error
	// SetConcern scores how big an issue a finding is 0–100, or marks it
	// unassessed with a nil score (see findings.Finding.Concern).
	SetConcern(id uint64, concern *int) error
	// Recording reports whether there is an engagement to record into.
	Recording() bool
}

// EntityStore is what the entity tools need from this process's Entity
// Explorer (see internal/entities). Like findings, entities belong to the
// open engagement, so they are the local process's, not routed through an
// Instance.
type EntityStore interface {
	ByTarget(targetID string) []entities.Entity
	Get(key string) (entities.Entity, bool)
	Lookup(scheme, host, path string) (entities.Entity, bool)
	Children(key string) []entities.Entity
	FlowIDs(key string) []uint64
	Discover(entities.Discovery) (entities.Entity, bool, error)
	ReplaceKV(key string, pairs []entities.Pair) (entities.Entity, error)
	Recording() bool
}

// Registry holds the tools and the dependencies they share.
type Registry struct {
	router   Router
	broker   *approval.Broker
	findings FindingStore
	entities EntityStore
	// files is the engagement's recovered files, wired by SetFiles rather
	// than passed to New so a process without one still builds a registry
	// (the file tools then say so).
	files FileStore

	// sourceMapExporter writes the recovered source of a local flow's source
	// map to disk; nil in a process without one (export_source_map then says
	// so). Injected rather than built in, because where the files go — the
	// open engagement's folder — is app's to decide.
	sourceMapExporter func(flowID uint64, by files.Source, author string) (files.Result, error)

	// listActions and runAction expose the running plugins' actions to the
	// action tools: the ones a flow is offered, and running one. Injected so
	// mcp need not import the plugin package; nil in a process with no
	// plugins (the tools then say so).
	listActions func(f capture.Flow) []PluginAction
	runAction   func(ctx context.Context, pluginName, actionID string, f capture.Flow) (message string, err error)

	tools  []*Tool
	byName map[string]*Tool
}

// PluginAction is a plugin action as the action tools present it.
type PluginAction struct {
	Plugin      string `json:"plugin"`
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// SetSourceMapExporter wires the function export_source_map calls to write a
// flow's recovered source to disk. Call it once, after New.
func (r *Registry) SetSourceMapExporter(fn func(flowID uint64, by files.Source, author string) (files.Result, error)) {
	r.sourceMapExporter = fn
}

// SetPluginActions wires the functions the action tools call to list the
// plugin actions offered for a flow and to run one. Call it once, after New.
func (r *Registry) SetPluginActions(
	list func(f capture.Flow) []PluginAction,
	run func(ctx context.Context, pluginName, actionID string, f capture.Flow) (string, error),
) {
	r.listActions = list
	r.runAction = run
}

// New builds the registry over router. broker may be nil when no caller will
// ever use ask mode (the gate then errors rather than panicking), and notes
// may be nil in a process with no findings store, in which case the findings
// tools report that rather than panicking.
func New(router Router, broker *approval.Broker, notes FindingStore, ents EntityStore) *Registry {
	r := &Registry{router: router, broker: broker, findings: notes, entities: ents, byName: map[string]*Tool{}}
	r.tools = tools()
	for _, t := range r.tools {
		r.byName[t.Name] = t
	}
	return r
}

// Tools returns the tool list, in a stable order.
func (r *Registry) Tools() []*Tool { return r.tools }

// Call runs the named tool for c and returns its result (a value ready to be
// JSON-encoded) or an error the caller can show the model.
func (r *Registry) Call(ctx context.Context, c Caller, name string, args json.RawMessage) (any, error) {
	t, ok := r.byName[name]
	if !ok {
		return nil, fmt.Errorf("unknown tool %q", name)
	}
	// A read-only caller may not invoke a tool that changes anything;
	// refuse before parsing or touching any instance.
	if (t.Mutates || t.Writes) && c.Policy == config.PolicyReadOnly {
		return nil, ErrReadOnly
	}
	if len(args) == 0 {
		args = json.RawMessage("{}")
	}
	return t.run(ctx, r, c, args)
}

// gate applies the caller's policy before a mutating tool acts: auto runs it,
// read_only refuses it, and ask blocks on the user through the broker.
func (r *Registry) gate(ctx context.Context, c Caller, tool, summary string) error {
	switch c.Policy {
	case config.PolicyAuto:
		return nil
	case config.PolicyReadOnly:
		return ErrReadOnly
	default: // PolicyAsk, and anything unrecognised, err on the side of asking
		if r.broker == nil {
			return errors.New("approval is required but no approval broker is configured")
		}
		ok, err := r.broker.Ask(ctx, approval.Request{
			Session: c.Session,
			Client:  c.Client,
			Tool:    tool,
			Summary: summary,
		})
		if err != nil {
			return err
		}
		if !ok {
			return ErrDenied
		}
		return nil
	}
}

// notes returns the findings store, or an error for a process without one.
func (r *Registry) notes() (FindingStore, error) {
	if r.findings == nil {
		return nil, ErrNoFindings
	}
	return r.findings, nil
}

// entityStore returns the local Entity Explorer, or an error when there is
// none.
func (r *Registry) entityStore() (EntityStore, error) {
	if r.entities == nil {
		return nil, ErrNoEntities
	}
	return r.entities, nil
}

// instanceByID returns the named instance, or the local one when id is empty.
func (r *Registry) instanceByID(id string) (instance.Instance, string, error) {
	if id == "" {
		id = r.router.LocalID()
	}
	inst, ok := r.router.Get(id)
	if !ok {
		return nil, "", fmt.Errorf("no instance %q is reachable", id)
	}
	return inst, id, nil
}
