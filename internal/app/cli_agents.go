package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"reproxy/internal/agent"
	"reproxy/internal/cli"
	"reproxy/internal/elide"
)

// The agent namespaces: agents (the chat sessions watching the traffic),
// approvals (what an agent is waiting for a yes or no on), mcp (the server
// external agents attach to) and focus (what the user is looking at, which
// is how "this request" resolves).
//
// They drive agent.Manager, approval.Broker and mcp.Manager as the panels
// do; a session started here shows in the Agents panel, and one started
// there can be sent to from here.

// turnWait is how long "agents send" waits for the answer before leaving
// the turn running. It is under the command line's own timeout, so the
// answer reaches the user rather than the connection ending first.
const turnWait = 4 * time.Minute

// ---- agents -----------------------------------------------------------------

func (a *App) agentCommands() []cli.Command {
	session := cli.Flag{Name: "session", Type: cli.FlagString, Required: true, Placeholder: "id",
		Description: "the session's id, as \"agents ls\" lists it"}
	return []cli.Command{
		{
			Namespace: "agents", Action: "ls",
			Summary: "the agent chat sessions in this instance", Run: a.cmdAgentsList,
		},
		{
			Namespace: "agents", Action: "profiles",
			Summary:     "the configured models",
			Description: "A profile is a vendor, a model and its key. The first with reviewing on is the one that scores new findings.",
			Run:         a.cmdAgentsProfiles,
		},
		{
			Namespace: "agents", Action: "new",
			Summary: "start a session",
			Description: "In ask mode the agent stops for a yes or no before anything that sends or changes traffic " +
				"(\"reproxy approvals ls\"); in auto it acts within the scope rule without asking.",
			Flags: []cli.Flag{
				{Name: "profile", Type: cli.FlagString, Placeholder: "id",
					Description: "which model to use (default: the first profile)"},
				{Name: "title", Type: cli.FlagString, Placeholder: "text", Description: "what to call it"},
				{Name: "mode", Type: cli.FlagString, Default: string(agent.ModeAsk), Placeholder: "mode",
					Description: "ask or auto"},
			},
			Run: a.cmdAgentsNew,
		},
		{
			Namespace: "agents", Action: "send",
			Summary: "send a message and show the answer",
			Description: "The turn runs with the same tools the panel gives the agent, so it can read traffic, " +
				"replay requests and record findings. --no-wait leaves it running and returns at once.",
			Flags: []cli.Flag{
				session,
				{Name: "text", Type: cli.FlagString, Required: true, Placeholder: "text",
					Description: "the message, or \"-\" for the piped input"},
				{Name: "wait", Type: cli.FlagBool, Default: "true", Description: "wait for the turn to finish"},
			},
			Run: a.cmdAgentsSend,
		},
		{
			Namespace: "agents", Action: "get",
			Summary: "a session's transcript",
			Flags:   []cli.Flag{session, limitFlag("20"), {Name: "tools", Type: cli.FlagBool, Description: "include the tool calls"}},
			Run:     a.cmdAgentsGet,
		},
		{
			Namespace: "agents", Action: "stop",
			Summary: "interrupt the turn a session is running", Flags: []cli.Flag{session},
			Run: a.cmdAgentsStop,
		},
		{
			Namespace: "agents", Action: "close",
			Summary: "close a session and forget its transcript", Flags: []cli.Flag{session},
			Run: a.cmdAgentsClose,
		},
	}
}

func (a *App) cmdAgentsList(_ context.Context, _ *cli.Input) (cli.Result, error) {
	list := a.agents.List()
	rows := make([][]string, 0, len(list))
	for _, s := range list {
		rows = append(rows, []string{s.ID, string(s.Status), string(s.Mode),
			a.profileName(s.ProfileID), elide.Runes(s.Title, 50)})
	}
	return listing([]string{"ID", "STATUS", "MODE", "MODEL", "TITLE"}, rows,
		"No agent session is open.", list)
}

// profileName is a profile's name, or its ID when it has been removed.
func (a *App) profileName(id string) string {
	if p, ok := a.AgentConfig().Profile(id); ok {
		return p.Name
	}
	return id
}

func (a *App) cmdAgentsProfiles(_ context.Context, _ *cli.Input) (cli.Result, error) {
	cfg := a.AgentConfig()
	rows := make([][]string, 0, len(cfg.Profiles))
	for _, p := range cfg.Profiles {
		key := "no key"
		if a.APIKey(p.ID) != "" {
			key = "key set"
		}
		rows = append(rows, []string{p.ID, p.Name, p.Vendor, p.Model, key, yesNo(p.ReviewsFindings())})
	}
	return listing([]string{"ID", "NAME", "VENDOR", "MODEL", "KEY", "REVIEWS"}, rows,
		"No agent profile is configured (Settings → Agents).", cfg.Profiles)
}

func (a *App) cmdAgentsNew(_ context.Context, in *cli.Input) (cli.Result, error) {
	profile := in.String("profile")
	if profile == "" {
		cfg := a.AgentConfig()
		if len(cfg.Profiles) == 0 {
			return cli.Result{}, fmt.Errorf("no agent profile is configured (Settings → Agents)")
		}
		profile = cfg.Profiles[0].ID
	}
	if _, ok := a.AgentConfig().Profile(profile); !ok {
		return cli.Result{}, fmt.Errorf("no agent profile %q; \"reproxy agents profiles\" lists them", profile)
	}
	sum := a.agents.Create(profile, in.String("title"))
	mode := agent.Mode(strings.ToLower(in.String("mode")))
	switch mode {
	case agent.ModeAsk, agent.ModeAuto:
		if err := a.agents.SetMode(sum.ID, mode); err != nil {
			return cli.Result{}, err
		}
	default:
		a.agents.Close(sum.ID)
		return cli.Result{}, fmt.Errorf("--mode takes ask or auto, not %q", in.String("mode"))
	}
	sum.Mode = mode
	return cli.Data(fmt.Sprintf("Session %s with %s, in %s mode.\n", sum.ID, a.profileName(profile), mode), sum)
}

func (a *App) cmdAgentsSend(ctx context.Context, in *cli.Input) (cli.Result, error) {
	id := in.String("session")
	if _, ok := a.agents.Get(id); !ok {
		return cli.Result{}, fmt.Errorf("no agent session %q; \"reproxy agents ls\" lists them", id)
	}
	text := in.Text("text")
	if strings.TrimSpace(text) == "" {
		return cli.Result{}, fmt.Errorf("--text was given nothing to send")
	}
	// Subscribe before sending, so a turn that finishes at once is not
	// missed between the send and the wait.
	done := make(chan agent.Status, 1)
	unsubscribe := a.agents.Subscribe(func(e agent.Event) {
		if e.Kind == agent.SessionChanged && e.Summary.ID == id && e.Summary.Status != agent.StatusRunning {
			select {
			case done <- e.Summary.Status:
			default:
			}
		}
	})
	defer unsubscribe()

	before := len(messagesOf(a, id))
	if err := a.agents.Send(id, text); err != nil {
		return cli.Result{}, err
	}
	if !in.Bool("wait") {
		return cli.Text("Sent. The turn is running: \"reproxy agents get --session %s\".\n", id), nil
	}
	select {
	case <-done:
	case <-ctx.Done():
		return cli.Result{}, ctx.Err()
	case <-time.After(turnWait):
		return cli.Result{}, fmt.Errorf("the turn is still running after %s: \"reproxy agents get --session %s\"",
			turnWait, id)
	}
	msgs := messagesOf(a, id)
	if before > len(msgs) {
		before = 0
	}
	said := transcript(msgs[before:], false)
	if strings.TrimSpace(said) == "" {
		said = "(the agent said nothing)"
	}
	return cli.Data(said+"\n", msgs[before:])
}

// messagesOf is a session's transcript, or nothing if it has gone.
func messagesOf(a *App, id string) []agent.Message {
	s, ok := a.agents.Get(id)
	if !ok {
		return nil
	}
	return s.Messages
}

// transcript renders messages the way the panel shows them.
func transcript(msgs []agent.Message, tools bool) string {
	var b strings.Builder
	for _, m := range msgs {
		if m.Tool != nil {
			if tools {
				fmt.Fprintf(&b, "[tool] %s\n", m.Tool.Name)
			}
			continue
		}
		if strings.TrimSpace(m.Text) == "" {
			continue
		}
		switch m.Role {
		case agent.RoleUser:
			fmt.Fprintf(&b, "you: %s\n", m.Text)
		default:
			fmt.Fprintf(&b, "%s\n", m.Text)
		}
	}
	return strings.TrimRight(b.String(), "\n")
}

func (a *App) cmdAgentsGet(_ context.Context, in *cli.Input) (cli.Result, error) {
	id := in.String("session")
	s, ok := a.agents.Get(id)
	if !ok {
		return cli.Result{}, fmt.Errorf("no agent session %q", id)
	}
	msgs := s.Messages
	if limit := in.Int("limit"); limit > 0 && len(msgs) > limit {
		msgs = msgs[len(msgs)-limit:]
	}
	head := fmt.Sprintf("%s · %s · %s · %s", s.ID, s.Status, s.Mode, s.Title)
	return cli.Data(lines(head, transcript(msgs, in.Bool("tools"))), s)
}

func (a *App) cmdAgentsStop(_ context.Context, in *cli.Input) (cli.Result, error) {
	id := in.String("session")
	if _, ok := a.agents.Get(id); !ok {
		return cli.Result{}, fmt.Errorf("no agent session %q", id)
	}
	a.agents.Stop(id)
	return cli.Text("Stopped the turn in %s.\n", id), nil
}

func (a *App) cmdAgentsClose(_ context.Context, in *cli.Input) (cli.Result, error) {
	id := in.String("session")
	if _, ok := a.agents.Get(id); !ok {
		return cli.Result{}, fmt.Errorf("no agent session %q", id)
	}
	a.agents.Close(id)
	return cli.Text("Closed %s.\n", id), nil
}

// ---- approvals --------------------------------------------------------------

func (a *App) approvalCommands() []cli.Command {
	id := cli.Flag{Name: "id", Type: cli.FlagString, Placeholder: "id",
		Description: "the request's id, as \"approvals ls\" lists it"}
	return []cli.Command{
		{
			Namespace: "approvals", Action: "ls",
			Summary:     "what an agent is waiting on",
			Description: "An agent in ask mode stops before anything that sends or changes traffic until it is answered.",
			Run:         a.cmdApprovalsList,
		},
		{
			Namespace: "approvals", Action: "approve",
			Summary: "let it go ahead",
			Flags:   []cli.Flag{id, {Name: "all", Type: cli.FlagBool, Description: "every pending request"}},
			Run:     a.cmdApprovalsApprove,
		},
		{
			Namespace: "approvals", Action: "deny",
			Summary: "refuse it",
			Flags:   []cli.Flag{id, {Name: "all", Type: cli.FlagBool, Description: "every pending request"}},
			Run:     a.cmdApprovalsDeny,
		},
	}
}

func (a *App) cmdApprovalsList(_ context.Context, _ *cli.Input) (cli.Result, error) {
	pending := a.broker.Pending()
	rows := make([][]string, 0, len(pending))
	for _, r := range pending {
		who := r.Session
		if who == "" {
			who = r.Client
		}
		rows = append(rows, []string{r.ID, who, r.Tool, since(r.Created), elide.Runes(r.Summary, 60)})
	}
	return listing([]string{"ID", "WHO", "TOOL", "WAITING", "WHAT"}, rows,
		"Nothing is waiting for a decision.", pending)
}

func (a *App) decideApprovals(in *cli.Input, approve bool) (cli.Result, error) {
	word := "Denied"
	if approve {
		word = "Approved"
	}
	if in.Bool("all") {
		pending := a.broker.Pending()
		for _, r := range pending {
			if err := a.broker.Decide(r.ID, approve); err != nil {
				return cli.Result{}, err
			}
		}
		return cli.Text("%s %s.\n", word, count(len(pending), "request", "requests")), nil
	}
	id := in.String("id")
	if id == "" {
		return cli.Result{}, fmt.Errorf("give --id, or --all for everything pending")
	}
	if err := a.broker.Decide(id, approve); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("%s %s.\n", word, id), nil
}

func (a *App) cmdApprovalsApprove(_ context.Context, in *cli.Input) (cli.Result, error) {
	return a.decideApprovals(in, true)
}

func (a *App) cmdApprovalsDeny(_ context.Context, in *cli.Input) (cli.Result, error) {
	return a.decideApprovals(in, false)
}

// ---- mcp --------------------------------------------------------------------

func (a *App) mcpCommands() []cli.Command {
	return []cli.Command{
		{
			Namespace: "mcp", Action: "status",
			Summary:     "whether the MCP server is listening",
			Description: "It runs on the process holding the hub role, and reaches every instance through it.",
			Run:         a.cmdMCPStatus,
		},
		{Namespace: "mcp", Action: "start", Summary: "start the MCP server", Run: a.cmdMCPStart},
		{Namespace: "mcp", Action: "stop", Summary: "stop the MCP server", Run: a.cmdMCPStop},
		{
			Namespace: "mcp", Action: "token",
			Summary: "the bearer token MCP clients must send",
			Description: "Regenerating it breaks every client configured with the old one, and the command line " +
				"itself reads the same token.",
			Flags: []cli.Flag{
				{Name: "regenerate", Type: cli.FlagBool, Description: "replace it with a new one"},
				yesFlag("every client configured with the old token stops working"),
			},
			Run: a.cmdMCPToken,
		},
	}
}

func (a *App) cmdMCPStatus(_ context.Context, _ *cli.Input) (cli.Result, error) {
	st := a.MCPStatus()
	switch {
	case st.Error != "":
		return cli.Data(fmt.Sprintf("The MCP server is not running: %s\n", st.Error), st)
	case st.Running:
		return cli.Data(fmt.Sprintf("The MCP server is listening on %s.\n", st.Addr), st)
	}
	return cli.Data("The MCP server is not running.\n", st)
}

func (a *App) cmdMCPStart(_ context.Context, _ *cli.Input) (cli.Result, error) {
	if err := a.StartMCP(); err != nil {
		return cli.Result{}, err
	}
	return a.cmdMCPStatus(context.Background(), nil)
}

func (a *App) cmdMCPStop(_ context.Context, _ *cli.Input) (cli.Result, error) {
	if err := a.StopMCP(); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("The MCP server is stopped.\n"), nil
}

func (a *App) cmdMCPToken(_ context.Context, in *cli.Input) (cli.Result, error) {
	if in.Bool("regenerate") {
		if err := confirmed(in, "every MCP client configured with the old token stops working"); err != nil {
			return cli.Result{}, err
		}
		tok, err := a.RegenerateMCPToken()
		if err != nil {
			return cli.Result{}, err
		}
		return cli.Data(tok+"\n", map[string]string{"token": tok})
	}
	tok, err := a.MCPToken()
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Data(tok+"\n", map[string]string{"token": tok})
}

// ---- focus ------------------------------------------------------------------

func (a *App) focusCommands() []cli.Command {
	return []cli.Command{{
		Namespace: "focus", Action: "get",
		Summary: "what the user is looking at",
		Description: "This is how \"this request\" resolves for an agent: the flow selected in the window, " +
			"or the Replay session last worked in.",
		Run: a.cmdFocusGet,
	}}
}

func (a *App) cmdFocusGet(_ context.Context, _ *cli.Input) (cli.Result, error) {
	s := a.focus.Snapshot()
	var said []string
	if s.SelectedFlow > 0 {
		line := "flow " + strconv.FormatUint(s.SelectedFlow, 10)
		if f, ok := a.store.Get(s.SelectedFlow); ok {
			line += fmt.Sprintf(": %s %s", f.Method, f.URL())
		}
		said = append(said, line)
	}
	if s.ReplaySession > 0 {
		said = append(said, fmt.Sprintf("replay session %d", s.ReplaySession))
	}
	if len(said) == 0 {
		return cli.Data("Nothing is selected.\n", s)
	}
	if s.Recent != "" {
		said = append(said, "last touched: the "+s.Recent)
	}
	return cli.Data(lines(said...), s)
}
