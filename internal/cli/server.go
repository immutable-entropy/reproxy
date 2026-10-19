package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
)

// The command line reaches a running reproxy process the way an agent does:
// over loopback HTTP, through the hub, with the shared bearer token. The
// client posts a request to the hub; the hub picks the instance (the one
// named with --instance, or its own) and asks it to run the command. The
// work happens in that process, against its live stores and its loaded
// plugins — nothing here opens the data directory or starts a plugin
// runtime of its own.

// HubPath is where HubHandler is mounted, under hub.HubPrefix.
const HubPath = "cli"

// HubRequest is what the client posts.
type HubRequest struct {
	// Instance is the instance ID to run in; empty means the hub's own.
	Instance string  `json:"instance,omitempty"`
	Request  Request `json:"request"`
}

// HubReply is what comes back. Error is set when the command (or reaching
// it) failed outright; a command that merely ran and found nothing answers
// with a Result like any other.
type HubReply struct {
	// Instance is the instance that ran it, so the client can say where
	// the answer came from.
	Instance string  `json:"instance,omitempty"`
	Result   *Result `json:"result,omitempty"`
	Error    string  `json:"error,omitempty"`
}

// Runner is one instance, as the hub reaches it. *instance.Local and
// *instance.Remote both satisfy it; the interface is here so that cli
// doesn't import instance (instance imports cli, for the types above).
type Runner interface {
	RunCommand(ctx context.Context, req Request) (Result, error)
}

// Resolver finds the instance a request names. It returns the instance and
// the ID it resolved to.
type Resolver func(id string) (Runner, string, error)

// HubHandler serves POST /hub/cli. The hub checks the bearer token before
// any handler it was given runs, so there is no authentication here.
func HubHandler(resolve Resolver) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST only", http.StatusMethodNotAllowed)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req HubRequest
		if err := json.Unmarshal(body, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		reply := HubReply{}
		runner, id, err := resolve(req.Instance)
		reply.Instance = id
		if err == nil {
			var res Result
			res, err = runner.RunCommand(r.Context(), req.Request)
			if err == nil {
				reply.Result = &res
			}
		}
		if err != nil {
			reply.Error = Message(err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(reply)
	})
}
