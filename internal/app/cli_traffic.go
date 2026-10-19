package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"reproxy/internal/capture"
	"reproxy/internal/cli"
	"reproxy/internal/elide"
	"reproxy/internal/instance"
	"reproxy/internal/intercept"
	"reproxy/internal/replay"
)

// The traffic namespaces: flows, intercept, replays, websockets and dns.
//
// They read and drive this process through instance.Local — the same object
// an agent reaches over MCP — so `reproxy flows ls` and an agent's
// list_flows answer from one implementation, filters, paging and all.

// cliOwner attributes Replay sessions and sends made from the command line.
// It is not an agent prefix, so replay.SourceOf makes these flows "replay",
// like the user's own.
const cliOwner replay.Owner = "cli"

// ---- flows ------------------------------------------------------------------

func (a *App) flowCommands() []cli.Command {
	query := []cli.Flag{
		{Name: "filter", Type: cli.FlagString, Placeholder: "text",
			Description: "keep the flows matching this text"},
		{Name: "regex", Type: cli.FlagBool, Description: "read --filter as a regular expression"},
		{Name: "in", Type: cli.FlagString, Default: instance.InSummary, Placeholder: "where",
			Description: "what --filter searches: summary, headers, body or all"},
		{Name: "protocol", Type: cli.FlagString, Placeholder: "p",
			Description: "keep only http or only dns"},
		limitFlag("50"),
		{Name: "after", Type: cli.FlagNumber, Placeholder: "id",
			Description: "start after this flow id, for the next page"},
		{Name: "newest", Type: cli.FlagBool, Description: "newest first"},
	}
	return []cli.Command{
		{
			Namespace: "flows", Action: "ls",
			Summary:     "list the captured requests",
			Description: "The same filter the request list uses, so a filter that works in the window works here.",
			Flags:       query,
			Run:         a.cmdFlowsList,
		},
		{
			Namespace: "flows", Action: "get",
			Summary:     "one flow, with its request and response",
			Description: "Bodies are decoded (chunked framing and gzip, deflate, brotli or zstd undone) exactly as the window and agents read them, never reformatted.",
			Flags: []cli.Flag{
				flowFlag("to show"),
				{Name: "part", Type: cli.FlagString, Default: "both", Placeholder: "part",
					Description: "request, response or both"},
				{Name: "head", Type: cli.FlagBool, Description: "headers only, no body"},
			},
			Run: a.cmdFlowsGet,
		},
		{
			Namespace: "flows", Action: "export",
			Summary:     "write flows out as JSON, to redirect into a file",
			Description: "Takes the same filters as ls. Each flow carries its messages as text, decoded the way `flows get` shows them.",
			Flags:       append([]cli.Flag{{Name: "bodies", Type: cli.FlagBool, Default: "true", Description: "include the message bodies"}}, query...),
			Run:         a.cmdFlowsExport,
		},
		{
			Namespace: "flows", Action: "clear",
			Summary:     "delete every captured request",
			Description: "They go from the list and from the engagement's archive on disk. The Explorer and the findings are kept.",
			Flags:       []cli.Flag{yesFlag("this cannot be undone")},
			Run:         a.cmdFlowsClear,
		},
		{
			Namespace: "flows", Action: "open",
			Summary:     "select a flow in the window",
			Description: "What the user is looking at is what \"this request\" means to an agent, so this is how a command hands one over.",
			Flags:       []cli.Flag{flowFlag("to select")},
			Run:         a.cmdFlowsOpen,
		},
	}
}

// flowQuery reads the listing flags every flow command shares.
func flowQuery(in *cli.Input) (instance.FlowQuery, error) {
	q := instance.FlowQuery{
		Filter: in.String("filter"),
		Regex:  in.Bool("regex"),
		In:     in.String("in"),
		Limit:  in.Int("limit"),
		After:  uint64(in.Int("after")),
		Newest: in.Bool("newest"),
	}
	switch q.In {
	case instance.InSummary, instance.InHeaders, instance.InBody, instance.InAll:
	default:
		return q, fmt.Errorf("--in takes summary, headers, body or all, not %q", q.In)
	}
	switch p := strings.ToLower(in.String("protocol")); p {
	case "", instance.ProtocolHTTP, instance.ProtocolDNS:
		q.Protocol = p
	default:
		return q, fmt.Errorf("--protocol takes http or dns, not %q", p)
	}
	return q, nil
}

func (a *App) cmdFlowsList(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	q, err := flowQuery(in)
	if err != nil {
		return cli.Result{}, err
	}
	page, err := local.ListFlows(ctx, q)
	if err != nil {
		return cli.Result{}, err
	}
	rows := make([][]string, 0, len(page.Flows))
	for _, f := range page.Flows {
		status := strconv.Itoa(f.Status)
		if f.Status == 0 {
			status = f.State
		}
		if f.Error != "" {
			status = "failed"
		}
		rows = append(rows, []string{
			strconv.FormatUint(f.ID, 10), f.Method, status,
			capture.FormatBytes(f.RespSize), f.Source, elide.Runes(f.URL, 70),
		})
	}
	res, err := listing([]string{"ID", "METHOD", "STATUS", "SIZE", "SOURCE", "URL"}, rows,
		"No request matches.", page)
	if err != nil || len(rows) == 0 {
		return res, err
	}
	res.Output += fmt.Sprintf("\n%d of %s", len(page.Flows), count(page.Matched, "match", "matches"))
	if page.More {
		res.Output += fmt.Sprintf("; more with --after %d", page.Flows[len(page.Flows)-1].ID)
	}
	res.Output += fmt.Sprintf(" (%s captured)\n", count(page.Total, "flow", "flows"))
	return res, nil
}

func (a *App) cmdFlowsGet(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	f, err := local.GetFlow(ctx, uint64(in.Int("flow")))
	if err != nil {
		return cli.Result{}, err
	}
	part := strings.ToLower(in.String("part"))
	if part != "request" && part != "response" && part != "both" {
		return cli.Result{}, fmt.Errorf("--part takes request, response or both, not %q", part)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", f.Method, f.URL())
	fmt.Fprintf(&b, "%s · %s · %s\n", instance.StateName(f.State), f.Started.Format("2006-01-02 15:04:05"), f.Source)
	if f.Error != "" {
		fmt.Fprintf(&b, "error: %s\n", f.Error)
	}
	for _, m := range []struct {
		name string
		msg  capture.Message
	}{{"request", f.Request}, {"response", f.Response}} {
		if part != "both" && part != m.name {
			continue
		}
		if len(m.msg.Raw) == 0 {
			continue
		}
		text := capture.DisplayText(m.msg)
		if in.Bool("head") {
			text, _, _ = strings.Cut(text, "\r\n\r\n")
			text, _, _ = strings.Cut(text, "\n\n")
		}
		fmt.Fprintf(&b, "\n--- %s ---\n%s\n", m.name, strings.TrimRight(text, "\n"))
	}
	return cli.Data(b.String(), f)
}

// exportedFlow is a flow as `flows export` writes it: the summary fields,
// with each message as the text the window shows.
type exportedFlow struct {
	instance.FlowSummary
	Request  string `json:"request,omitempty"`
	Response string `json:"response,omitempty"`
}

func (a *App) cmdFlowsExport(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	q, err := flowQuery(in)
	if err != nil {
		return cli.Result{}, err
	}
	page, err := local.ListFlows(ctx, q)
	if err != nil {
		return cli.Result{}, err
	}
	out := make([]exportedFlow, 0, len(page.Flows))
	for _, s := range page.Flows {
		e := exportedFlow{FlowSummary: s}
		if in.Bool("bodies") {
			if f, err := local.GetFlow(ctx, s.ID); err == nil {
				e.Request, e.Response = capture.DisplayText(f.Request), capture.DisplayText(f.Response)
			}
		}
		out = append(out, e)
	}
	// The JSON is the point here, so it is what the text says too.
	res, err := cli.Data("", out)
	if err != nil {
		return res, err
	}
	res.Output = string(res.JSON) + "\n"
	return res, nil
}

func (a *App) cmdFlowsClear(_ context.Context, in *cli.Input) (cli.Result, error) {
	n := a.store.Len()
	if n == 0 {
		return cli.Text("There is nothing to clear.\n"), nil
	}
	where := "the archive"
	if e, open := a.engagements.Current(); open {
		where = e.Label()
	}
	if err := confirmed(in, fmt.Sprintf("this deletes %s from the list and from %s on disk",
		count(n, "request", "requests"), where)); err != nil {
		return cli.Result{}, err
	}
	a.store.Clear()
	return cli.Text("Cleared %s. The Explorer and the findings are kept.\n", count(n, "request", "requests")), nil
}

func (a *App) cmdFlowsOpen(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	id := uint64(in.Int("flow"))
	f, err := local.GetFlow(ctx, id)
	if err != nil {
		return cli.Result{}, err
	}
	a.focus.SelectFlow(id)
	return cli.Text("Selected %d: %s %s\n", id, f.Method, f.URL()), nil
}

// ---- intercept --------------------------------------------------------------

func (a *App) interceptCommands() []cli.Command {
	return []cli.Command{
		{
			Namespace: "intercept", Action: "status",
			Summary:     "whether interception is on, and what it is holding",
			Description: "What a target holds is set per target (\"reproxy targets intercept\"); this is the master switch and the queue.",
			Run:         a.cmdInterceptStatus,
		},
		{
			Namespace: "intercept", Action: "on",
			Summary: "switch interception on", Run: a.cmdInterceptOn,
		},
		{
			Namespace: "intercept", Action: "off",
			Summary:     "switch interception off",
			Description: "Anything held is released as it was.",
			Run:         a.cmdInterceptOff,
		},
		{
			Namespace: "intercept", Action: "held",
			Summary: "the messages waiting for a decision", Run: a.cmdInterceptHeld,
		},
		{
			Namespace: "intercept", Action: "show",
			Summary:     "the held message, as text you can edit and forward",
			Flags:       []cli.Flag{flowFlag("whose held message to show")},
			Description: "This is the exact text \"intercept forward --edit\" takes back.",
			Run:         a.cmdInterceptShow,
		},
		{
			Namespace: "intercept", Action: "forward",
			Summary: "release a held message, edited or as it is",
			Description: "With --edit the whole message is replaced: pass it as text, or as \"-\" to use what was piped in " +
				"(reproxy intercept show --flow 42 | sed … | reproxy intercept forward --flow 42 --edit -).",
			Flags: []cli.Flag{
				flowFlag("to release"),
				{Name: "edit", Type: cli.FlagString, Placeholder: "text",
					Description: "replace the message with this, or \"-\" for the piped input"},
			},
			Run: a.cmdInterceptForward,
		},
		{
			Namespace: "intercept", Action: "drop",
			Summary: "drop a held message", Flags: []cli.Flag{flowFlag("to drop")},
			Run: a.cmdInterceptDrop,
		},
	}
}

func (a *App) cmdInterceptStatus(ctx context.Context, _ *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	st, err := local.Intercept(ctx)
	if err != nil {
		return cli.Result{}, err
	}
	state := "off"
	if st.Enabled {
		state = "on"
	}
	return cli.Data(lines(
		"Interception is "+state+".",
		fmt.Sprintf("%s held.", count(len(st.Held), "message", "messages")),
	), st)
}

func (a *App) setIntercept(ctx context.Context, on bool) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	if err := local.SetIntercept(ctx, on); err != nil {
		return cli.Result{}, err
	}
	return a.cmdInterceptStatus(ctx, nil)
}

func (a *App) cmdInterceptOn(ctx context.Context, _ *cli.Input) (cli.Result, error) {
	return a.setIntercept(ctx, true)
}

func (a *App) cmdInterceptOff(ctx context.Context, _ *cli.Input) (cli.Result, error) {
	return a.setIntercept(ctx, false)
}

func (a *App) cmdInterceptHeld(ctx context.Context, _ *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	st, err := local.Intercept(ctx)
	if err != nil {
		return cli.Result{}, err
	}
	rows := make([][]string, 0, len(st.Held))
	for _, h := range st.Held {
		rows = append(rows, []string{
			strconv.FormatUint(h.FlowID, 10), h.Direction, h.Method,
			capture.FormatBytes(int64(h.Size)), elide.Runes(h.URL, 70),
		})
	}
	return listing([]string{"FLOW", "DIRECTION", "METHOD", "SIZE", "URL"}, rows,
		"Nothing is held.", st.Held)
}

func (a *App) cmdInterceptShow(_ context.Context, in *cli.Input) (cli.Result, error) {
	msg, ok := a.intercept.Message(uint64(in.Int("flow")))
	if !ok {
		return cli.Result{}, instance.ErrNotHeld
	}
	return cli.Data(intercept.EditableText(msg.Raw), map[string]any{
		"flow_id": msg.FlowID, "direction": msg.Dir.String(), "text": intercept.EditableText(msg.Raw),
	})
}

func (a *App) cmdInterceptForward(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	id := uint64(in.Int("flow"))
	var edited *string
	if in.Given("edit") {
		text := in.Text("edit")
		if strings.TrimSpace(text) == "" {
			return cli.Result{}, fmt.Errorf("--edit was given nothing to replace the message with")
		}
		edited = &text
	}
	if err := local.Forward(ctx, id, edited); err != nil {
		return cli.Result{}, err
	}
	what := "Forwarded"
	if edited != nil {
		what = "Forwarded edited"
	}
	return cli.Text("%s message on flow %d.\n", what, id), nil
}

func (a *App) cmdInterceptDrop(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	id := uint64(in.Int("flow"))
	if err := local.Drop(ctx, id); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Dropped flow %d.\n", id), nil
}

// ---- replays ----------------------------------------------------------------

func (a *App) replayCommands() []cli.Command {
	session := cli.Flag{Name: "session", Type: cli.FlagNumber, Required: true, Placeholder: "id",
		Description: "the replay session"}
	return []cli.Command{
		{
			Namespace: "replays", Action: "ls",
			Summary: "the replay sessions in this instance", Run: a.cmdReplaysList,
		},
		{
			Namespace: "replays", Action: "new",
			Summary:     "start a replay session from a captured flow",
			Description: "The session starts with that request, ready to edit and send. --addr sends it somewhere else without changing the request.",
			Flags: []cli.Flag{
				flowFlag("to replay"),
				{Name: "addr", Type: cli.FlagString, Placeholder: "host:port",
					Description: "connect here instead of where the flow went"},
			},
			Run: a.cmdReplaysNew,
		},
		{
			Namespace: "replays", Action: "get",
			Summary: "a session and what it has sent", Flags: []cli.Flag{session},
			Run: a.cmdReplaysGet,
		},
		{
			Namespace: "replays", Action: "send",
			Summary: "send the session's request and show the answer",
			Description: "With no --raw the last request is sent again. Replays don't go through the proxy, " +
				"so interception never holds them; they are recorded in the traffic list as \"replay\".",
			Flags: []cli.Flag{
				session,
				{Name: "raw", Type: cli.FlagString, Placeholder: "text",
					Description: "the whole request to send, or \"-\" for the piped input"},
			},
			Run: a.cmdReplaysSend,
		},
		{
			Namespace: "replays", Action: "rm",
			Summary: "close a replay session", Flags: []cli.Flag{session},
			Run: a.cmdReplaysRemove,
		},
	}
}

func (a *App) cmdReplaysList(ctx context.Context, _ *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	sessions, err := local.ReplayList(ctx)
	if err != nil {
		return cli.Result{}, err
	}
	rows := make([][]string, 0, len(sessions))
	for _, s := range sessions {
		// A listing's sessions come without their history (it can be long),
		// so the count and the last send are read from the registry.
		last := ""
		if full, ok := a.replays.Get(s.ID); ok {
			if e, sent := full.Last(); sent {
				last = fmt.Sprintf("%d %s", e.Response.StatusCode, since(e.At))
				if e.Err != "" {
					last = "failed " + since(e.At)
				}
			}
		}
		rows = append(rows, []string{
			strconv.FormatUint(s.ID, 10), string(s.Owner), s.Template.Method,
			elide.Runes(s.Template.URL, 50), strconv.Itoa(a.replays.Sends(s.ID)), last,
		})
	}
	return listing([]string{"ID", "OWNER", "METHOD", "URL", "SENDS", "LAST"}, rows,
		"No replay session is open.", sessions)
}

func (a *App) cmdReplaysNew(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	s, err := local.ReplayCreate(ctx, cliOwner, uint64(in.Int("flow")), in.String("addr"))
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Data(fmt.Sprintf("Replay session %d: %s %s\n", s.ID, s.Template.Method, s.Template.URL), s)
}

func (a *App) cmdReplaysGet(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	s, err := local.ReplayGet(ctx, uint64(in.Int("session")))
	if err != nil {
		return cli.Result{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Session %d: %s %s (%s)\n", s.ID, s.Template.Method, s.Template.URL, s.Owner)
	for _, e := range s.History {
		line := fmt.Sprintf("  #%d %s", e.Seq, since(e.At))
		if e.Err != "" {
			line += " failed: " + e.Err
		} else {
			line += fmt.Sprintf(" %d %s in %dms", e.Response.StatusCode,
				capture.FormatBytes(int64(e.Response.Size)), e.Response.Duration.Milliseconds())
		}
		b.WriteString(line + "\n")
	}
	if len(s.History) == 0 {
		b.WriteString("  nothing sent yet\n")
	}
	return cli.Data(b.String(), s)
}

func (a *App) cmdReplaysSend(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	id := uint64(in.Int("session"))
	raw := []byte(in.Text("raw"))
	if len(raw) == 0 {
		// Nothing given: send what the session holds — the last thing sent
		// from it, or the request it was made from.
		s, err := local.ReplayGet(ctx, id)
		if err != nil {
			return cli.Result{}, err
		}
		if last, ok := s.Last(); ok {
			raw = last.Request
		} else {
			raw = s.Template.Raw
		}
		if len(raw) == 0 {
			return cli.Result{}, fmt.Errorf("session %d holds no request to send: pass --raw", id)
		}
	}
	e, err := local.ReplaySend(ctx, id, raw, cliOwner)
	if err != nil {
		return cli.Result{}, err
	}
	if e.Err != "" {
		return cli.Data(fmt.Sprintf("Send #%d failed: %s\n", e.Seq, e.Err), e)
	}
	return cli.Data(fmt.Sprintf("#%d %d %s in %dms (flow %d)\n", e.Seq, e.Response.StatusCode,
		capture.FormatBytes(int64(e.Response.Size)), e.Response.Duration.Milliseconds(), e.FlowID), e)
}

func (a *App) cmdReplaysRemove(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	id := uint64(in.Int("session"))
	if _, err := local.ReplayGet(ctx, id); err != nil {
		return cli.Result{}, err
	}
	a.replays.Close(id)
	return cli.Text("Closed replay session %d.\n", id), nil
}

// ---- websockets -------------------------------------------------------------

func (a *App) websocketCommands() []cli.Command {
	return []cli.Command{{
		Namespace: "websockets", Action: "ls",
		Summary: "the messages of a flow's WebSocket connection",
		Description: "Payloads are shown as text where they are text. Sending a message on a live connection " +
			"is not something core can do yet, here or anywhere else.",
		Flags: []cli.Flag{
			flowFlag("whose connection to show"),
			limitFlag("100"),
			{Name: "direction", Type: cli.FlagString, Placeholder: "d",
				Description: "only sent or only received"},
		},
		Run: a.cmdWebSocketsList,
	}}
}

func (a *App) cmdWebSocketsList(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	f, err := local.GetFlow(ctx, uint64(in.Int("flow")))
	if err != nil {
		return cli.Result{}, err
	}
	want := strings.ToLower(in.String("direction"))
	if want != "" && want != "sent" && want != "received" {
		return cli.Result{}, fmt.Errorf("--direction takes sent or received, not %q", want)
	}
	limit := in.Int("limit")
	rows := make([][]string, 0, len(f.Messages))
	kept := make([]capture.WSMessage, 0, len(f.Messages))
	for _, m := range f.Messages {
		dir := m.Dir.String()
		if want != "" && !strings.EqualFold(dir, want) {
			continue
		}
		if len(rows) >= limit {
			break
		}
		note := ""
		switch {
		case m.Dropped:
			note = "dropped"
		case m.Edited:
			note = "edited"
		case m.Truncated:
			note = "truncated"
		}
		kept = append(kept, m)
		rows = append(rows, []string{
			strconv.Itoa(m.Seq), dir, m.Type.String(), capture.FormatBytes(int64(len(m.Data))),
			note, elide.Runes(strings.ReplaceAll(string(m.Data), "\n", " "), 60),
		})
	}
	return listing([]string{"SEQ", "DIRECTION", "TYPE", "SIZE", "", "PAYLOAD"}, rows,
		"That flow carries no WebSocket messages.", kept)
}

// ---- dns --------------------------------------------------------------------

func (a *App) dnsCommands() []cli.Command {
	return []cli.Command{
		{
			Namespace: "dns", Action: "ls",
			Summary: "the name lookups made for this instance",
			Description: "Lookups are flows like any other, so this is \"flows ls --protocol dns\" " +
				"with the answers brought forward. Only lookups a target covers are recorded.",
			Flags: []cli.Flag{
				{Name: "filter", Type: cli.FlagString, Placeholder: "text", Description: "keep the lookups matching this text"},
				limitFlag("50"),
				{Name: "newest", Type: cli.FlagBool, Description: "newest first"},
			},
			Run: a.cmdDNSList,
		},
		{
			Namespace: "dns", Action: "flush",
			Summary:     "empty the resolver's cache",
			Description: "Answers are cached for a minute. The cache is emptied anyway whenever the targets or the upstream chain change.",
			Run:         a.cmdDNSFlush,
		},
	}
}

func (a *App) cmdDNSList(ctx context.Context, in *cli.Input) (cli.Result, error) {
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	page, err := local.ListFlows(ctx, instance.FlowQuery{
		Filter: in.String("filter"), Limit: in.Int("limit"), Newest: in.Bool("newest"),
		Protocol: instance.ProtocolDNS,
	})
	if err != nil {
		return cli.Result{}, err
	}
	rows := make([][]string, 0, len(page.Flows))
	for _, f := range page.Flows {
		name := strings.TrimPrefix(f.URL, "dns://")
		rows = append(rows, []string{
			strconv.FormatUint(f.ID, 10), name, f.StatusText, elide.Runes(f.Answer, 50), since(f.Started),
		})
	}
	return listing([]string{"ID", "NAME", "RESULT", "ANSWER", "AGE"}, rows,
		"No lookup has been recorded. Only lookups a target covers are.", page)
}

func (a *App) cmdDNSFlush(_ context.Context, _ *cli.Input) (cli.Result, error) {
	if a.dns == nil {
		return cli.Result{}, fmt.Errorf("this instance has no resolver")
	}
	a.dns.Flush()
	return cli.Text("Flushed the resolver's cache.\n"), nil
}
