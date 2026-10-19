// Package instance is the set of operations an agent (in-app or through MCP)
// can perform on one running reproxy process: read its captured traffic,
// drive its interception, work in its Replay sessions and see what its user
// is looking at.
//
// Local implements it over the process's own core objects. Remote implements
// it over HTTP for another reproxy process, served by Handler, so the hub
// process can reach every instance through the same interface.
//
// Nothing here is on the proxy's hot path: it only reads the store and
// interceptor and calls their existing methods.
package instance

import (
	"context"
	"errors"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/cli"
	"reproxy/internal/focus"
	"reproxy/internal/replay"
)

// Errors with a fixed meaning across the HTTP boundary.
var (
	ErrNoFlow    = errors.New("no such flow")
	ErrNoSession = errors.New("no such replay session")
	ErrNotHeld   = errors.New("that message is not held")
)

// Info identifies a running instance.
type Info struct {
	// ID is random per process start, so handles minted by a process that
	// has exited can never match a new one.
	ID        string    `json:"id"`
	PID       int       `json:"pid"`
	ProxyAddr string    `json:"proxy_addr,omitempty"`
	ProxyErr  string    `json:"proxy_error,omitempty"`
	Started   time.Time `json:"started"`
}

// FlowSummary is a flow without its message bytes.
type FlowSummary struct {
	ID         uint64    `json:"id"`
	State      string    `json:"state"`
	Method     string    `json:"method"`
	URL        string    `json:"url"`
	Status     int       `json:"status,omitempty"`
	StatusText string    `json:"status_text,omitempty"`
	ReqSize    int64     `json:"request_size"`
	RespSize   int64     `json:"response_size,omitempty"`
	Started    time.Time `json:"started"`
	DurationMs int64     `json:"duration_ms,omitempty"`
	Edited     bool      `json:"edited,omitempty"`
	// Source is "replay" or "agent" for a request sent from a replay
	// session, and omitted for traffic that came through the proxy.
	Source string `json:"source,omitempty"`
	Error  string `json:"error,omitempty"`
	// Answer is a DNS lookup's addresses, on one line; a lookup's method is
	// "DNS" and its URL dns://name. It is omitted for HTTP.
	Answer string `json:"answer,omitempty"`
}

// Summarize describes f without its bytes.
func Summarize(f capture.Flow) FlowSummary {
	return FlowSummary{
		ID:         f.ID,
		State:      StateName(f.State),
		Method:     f.Method,
		URL:        f.URL(),
		Status:     f.StatusCode,
		StatusText: f.StatusText,
		ReqSize:    f.Request.Size,
		RespSize:   f.Response.Size,
		Started:    f.Started,
		DurationMs: f.Duration.Milliseconds(),
		Edited:     f.Edited(),
		Source:     string(f.Source),
		Error:      f.Error,
		Answer:     dnsAnswer(f),
	}
}

func dnsAnswer(f capture.Flow) string {
	if f.IsDNS() {
		return f.Path
	}
	return ""
}

// StateName is a flow state's name in tool output.
func StateName(s capture.FlowState) string {
	switch s {
	case capture.StatePending:
		return "pending"
	case capture.StateComplete:
		return "complete"
	case capture.StateFailed:
		return "failed"
	case capture.StateHeldRequest:
		return "held_request"
	case capture.StateHeldResponse:
		return "held_response"
	case capture.StateDropped:
		return "dropped"
	}
	return "unknown"
}

// Search scopes for FlowQuery.In.
const (
	// InSummary matches method, URL and status (the request-list filter).
	InSummary = "summary"
	// InHeaders matches the request and response start lines and headers.
	InHeaders = "headers"
	// InBody matches the request and response bodies.
	InBody = "body"
	// InAll matches the complete raw messages.
	InAll = "all"
)

// FlowQuery selects flows. The zero value lists the oldest flows.
type FlowQuery struct {
	Filter string `json:"filter,omitempty"`
	Regex  bool   `json:"regex,omitempty"`
	In     string `json:"in,omitempty"` // InSummary (default), InHeaders, InBody or InAll
	// After skips flows with IDs up to and including it, for paging.
	After uint64 `json:"after,omitempty"`
	// Limit caps the page; 0 means DefaultLimit, and it is capped at MaxLimit.
	Limit int `json:"limit,omitempty"`
	// Newest pages from the newest flow backwards (After then means
	// "before").
	Newest bool `json:"newest,omitempty"`
	// IDs, when set, keeps only flows with these IDs — how list_flows
	// lists the requests behind one entity.
	IDs []uint64 `json:"ids,omitempty"`
	// Protocol keeps only HTTP flows ("http") or only DNS lookups ("dns");
	// empty keeps both. It is applied before the page is cut, so a page is
	// full whichever is asked for.
	Protocol string `json:"protocol,omitempty"`
}

// Protocols for FlowQuery.Protocol.
const (
	ProtocolHTTP = "http"
	ProtocolDNS  = "dns"
)

// Page sizes for FlowQuery.
const (
	DefaultLimit = 50
	MaxLimit     = 200
)

// FlowPage is one page of matching flows.
type FlowPage struct {
	Flows []FlowSummary `json:"flows"`
	// Matched is how many flows match in total; Total is how many there are.
	Matched int  `json:"matched"`
	Total   int  `json:"total"`
	More    bool `json:"more"`
}

// Held is a message held by interception.
type Held struct {
	FlowID    uint64 `json:"flow_id"`
	Direction string `json:"direction"` // "request" or "response"
	Method    string `json:"method"`
	URL       string `json:"url"`
	Size      int    `json:"size"`
}

// InterceptState is whether interception is on and what it holds.
type InterceptState struct {
	Enabled bool   `json:"enabled"`
	Held    []Held `json:"held"`
}

// Instance is everything an agent can do in one reproxy process.
type Instance interface {
	Info(ctx context.Context) (Info, error)

	ListFlows(ctx context.Context, q FlowQuery) (FlowPage, error)
	GetFlow(ctx context.Context, id uint64) (capture.Flow, error)

	Intercept(ctx context.Context) (InterceptState, error)
	SetIntercept(ctx context.Context, enabled bool) error
	// WaitHeld returns the oldest held message, waiting up to timeout for
	// one to arrive if none is held; ok is false on timeout.
	WaitHeld(ctx context.Context, timeout time.Duration) (h Held, ok bool, err error)
	// Forward releases a held message; edited (the whole message as text,
	// see intercept.EditableText) replaces it when non-nil.
	Forward(ctx context.Context, flowID uint64, edited *string) error
	Drop(ctx context.Context, flowID uint64) error

	// ReplayCreate starts a Replay session from a captured flow. addr, when
	// not empty, replaces the flow's connect target; callers enforce the
	// scope rule before asking.
	ReplayCreate(ctx context.Context, owner replay.Owner, flowID uint64, addr string) (replay.Session, error)
	ReplaySend(ctx context.Context, sessionID uint64, raw []byte, by replay.Owner) (replay.Entry, error)
	ReplayGet(ctx context.Context, sessionID uint64) (replay.Session, error)
	ReplayList(ctx context.Context) ([]replay.Session, error)

	// Focus reports what the user had focused at the given moment (the
	// zero time: now), or focus.ErrTooOld once that is forgotten.
	Focus(ctx context.Context, at time.Time) (focus.Snapshot, error)

	// RunCommand runs one command-line invocation in this instance: a core
	// command against its own objects, a plugin's in the plugin's runtime.
	// See internal/cli.
	RunCommand(ctx context.Context, req cli.Request) (cli.Result, error)
}
