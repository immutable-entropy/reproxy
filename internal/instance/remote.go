package instance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/cli"
	"reproxy/internal/focus"
	"reproxy/internal/replay"
)

// The HTTP form of Instance: POST <prefix>/<method> with a JSON argument
// object, answered with {"result": ...} or {"error": "...", "code": "..."}.
// Every request carries the shared bearer token.

// PathPrefix is where Handler is mounted.
const PathPrefix = "/instance/"

type rpcReply struct {
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
	Code   string          `json:"code,omitempty"`
}

var errCodes = map[string]error{
	"no_flow":    ErrNoFlow,
	"no_session": ErrNoSession,
	"not_held":   ErrNotHeld,
	"focus_old":  focus.ErrTooOld,
}

func codeOf(err error) string {
	for code, e := range errCodes {
		if errors.Is(err, e) {
			return code
		}
	}
	return ""
}

type (
	idArgs        struct{ ID uint64 }
	interceptArgs struct{ Enabled bool }
	waitArgs      struct{ TimeoutMs int64 }
	focusArgs     struct{ At time.Time }
	forwardArgs   struct {
		ID     uint64
		Edited *string
	}
	createArgs struct {
		Owner  replay.Owner
		FlowID uint64
		Addr   string
	}
	sendArgs struct {
		ID  uint64
		Raw []byte
		By  replay.Owner
	}
	waitResult struct {
		Held Held
		OK   bool
	}
)

// rpc adapts a method that takes an argument object: it unmarshals the
// request body into A and calls fn. Every entry in Handler's table did
// this by hand, which was five lines each and one place per method for the
// unmarshal to be forgotten.
func rpc[A any](fn func(context.Context, A) (any, error)) func(context.Context, []byte) (any, error) {
	return func(ctx context.Context, b []byte) (any, error) {
		var a A
		if err := json.Unmarshal(b, &a); err != nil {
			return nil, err
		}
		return fn(ctx, a)
	}
}

// noArgs adapts a method that takes none, ignoring the body. R is the
// method's own result type, so the method can be passed by name.
func noArgs[R any](fn func(context.Context) (R, error)) func(context.Context, []byte) (any, error) {
	return func(ctx context.Context, _ []byte) (any, error) { return fn(ctx) }
}

// Handler serves inst over HTTP. authorized decides whether a request may
// proceed (it checks the bearer token).
func Handler(inst Instance, authorized func(*http.Request) bool) http.Handler {
	methods := map[string]func(ctx context.Context, body []byte) (any, error){
		"info": noArgs(inst.Info),
		"list_flows": rpc(func(ctx context.Context, q FlowQuery) (any, error) {
			return inst.ListFlows(ctx, q)
		}),
		"get_flow": rpc(func(ctx context.Context, a idArgs) (any, error) {
			return inst.GetFlow(ctx, a.ID)
		}),
		"intercept": noArgs(inst.Intercept),
		"set_intercept": rpc(func(ctx context.Context, a interceptArgs) (any, error) {
			return struct{}{}, inst.SetIntercept(ctx, a.Enabled)
		}),
		"wait_held": rpc(func(ctx context.Context, a waitArgs) (any, error) {
			h, ok, err := inst.WaitHeld(ctx, time.Duration(a.TimeoutMs)*time.Millisecond)
			return waitResult{h, ok}, err
		}),
		"forward": rpc(func(ctx context.Context, a forwardArgs) (any, error) {
			return struct{}{}, inst.Forward(ctx, a.ID, a.Edited)
		}),
		"drop": rpc(func(ctx context.Context, a idArgs) (any, error) {
			return struct{}{}, inst.Drop(ctx, a.ID)
		}),
		"replay_create": rpc(func(ctx context.Context, a createArgs) (any, error) {
			return inst.ReplayCreate(ctx, a.Owner, a.FlowID, a.Addr)
		}),
		"replay_send": rpc(func(ctx context.Context, a sendArgs) (any, error) {
			return inst.ReplaySend(ctx, a.ID, a.Raw, a.By)
		}),
		"replay_get": rpc(func(ctx context.Context, a idArgs) (any, error) {
			return inst.ReplayGet(ctx, a.ID)
		}),
		"replay_list": noArgs(inst.ReplayList),
		"focus": rpc(func(ctx context.Context, a focusArgs) (any, error) {
			return inst.Focus(ctx, a.At)
		}),
		"run_command": rpc(func(ctx context.Context, a cli.Request) (any, error) {
			return inst.RunCommand(ctx, a)
		}),
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		fn, ok := methods[strings.TrimPrefix(r.URL.Path, PathPrefix)]
		if !ok {
			http.NotFound(w, r)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 64<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if len(body) == 0 {
			body = []byte("{}")
		}
		result, err := fn(r.Context(), body)
		var reply rpcReply
		if err != nil {
			reply.Error, reply.Code = err.Error(), codeOf(err)
		} else if reply.Result, err = json.Marshal(result); err != nil {
			reply.Error = err.Error()
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reply)
	})
}

// Remote is an Instance in another reproxy process, reached over HTTP.
type Remote struct {
	base   string // http://host:port
	token  func() (string, error)
	client *http.Client
}

// NewRemote returns a client for the instance served at addr (host:port).
// token supplies the bearer token for each call.
func NewRemote(addr string, token func() (string, error)) *Remote {
	return &Remote{base: "http://" + addr, token: token, client: &http.Client{}}
}

func (r *Remote) call(ctx context.Context, method string, args, out any) error {
	body, err := json.Marshal(args)
	if err != nil {
		return err
	}
	tok, err := r.token()
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.base+PathPrefix+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Content-Type", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return fmt.Errorf("instance unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("instance: %s: %s", resp.Status, strings.TrimSpace(string(msg)))
	}
	var reply rpcReply
	if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
		return fmt.Errorf("instance: bad reply: %w", err)
	}
	if reply.Error != "" {
		if e, ok := errCodes[reply.Code]; ok {
			return e
		}
		return errors.New(reply.Error)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(reply.Result, out)
}

func (r *Remote) Info(ctx context.Context) (i Info, err error) {
	err = r.call(ctx, "info", struct{}{}, &i)
	return
}

func (r *Remote) ListFlows(ctx context.Context, q FlowQuery) (p FlowPage, err error) {
	err = r.call(ctx, "list_flows", q, &p)
	return
}

func (r *Remote) GetFlow(ctx context.Context, id uint64) (f capture.Flow, err error) {
	err = r.call(ctx, "get_flow", idArgs{id}, &f)
	return
}

func (r *Remote) Intercept(ctx context.Context) (s InterceptState, err error) {
	err = r.call(ctx, "intercept", struct{}{}, &s)
	return
}

func (r *Remote) SetIntercept(ctx context.Context, enabled bool) error {
	return r.call(ctx, "set_intercept", interceptArgs{enabled}, nil)
}

func (r *Remote) WaitHeld(ctx context.Context, timeout time.Duration) (Held, bool, error) {
	var res waitResult
	err := r.call(ctx, "wait_held", waitArgs{timeout.Milliseconds()}, &res)
	return res.Held, res.OK, err
}

func (r *Remote) Forward(ctx context.Context, flowID uint64, edited *string) error {
	return r.call(ctx, "forward", forwardArgs{flowID, edited}, nil)
}

func (r *Remote) Drop(ctx context.Context, flowID uint64) error {
	return r.call(ctx, "drop", idArgs{flowID}, nil)
}

func (r *Remote) ReplayCreate(ctx context.Context, owner replay.Owner, flowID uint64, addr string) (s replay.Session, err error) {
	err = r.call(ctx, "replay_create", createArgs{owner, flowID, addr}, &s)
	return
}

func (r *Remote) ReplaySend(ctx context.Context, sessionID uint64, raw []byte, by replay.Owner) (e replay.Entry, err error) {
	err = r.call(ctx, "replay_send", sendArgs{sessionID, raw, by}, &e)
	return
}

func (r *Remote) ReplayGet(ctx context.Context, sessionID uint64) (s replay.Session, err error) {
	err = r.call(ctx, "replay_get", idArgs{sessionID}, &s)
	return
}

func (r *Remote) ReplayList(ctx context.Context) (s []replay.Session, err error) {
	err = r.call(ctx, "replay_list", struct{}{}, &s)
	return
}

func (r *Remote) Focus(ctx context.Context, at time.Time) (s focus.Snapshot, err error) {
	err = r.call(ctx, "focus", focusArgs{at}, &s)
	return
}

func (r *Remote) RunCommand(ctx context.Context, req cli.Request) (res cli.Result, err error) {
	err = r.call(ctx, "run_command", req, &res)
	return
}
