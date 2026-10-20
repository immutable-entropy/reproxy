package app

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"reproxy/internal/capture"
	"reproxy/internal/cli"
	"reproxy/internal/crawl"
	"reproxy/internal/elide"
	"reproxy/internal/engagement"
	"reproxy/internal/target"
)

// The scope namespaces: targets (what is collected and held, and what is
// suppressed), engagements (the folder the work lives in, and its archive)
// and crawl (Autocrawl).
//
// Each one drives the same store the window does — target.Store,
// engagement.Manager, crawl.Crawler — so a change made here reaches the
// proxy, the Explorer and the open window at once.

// ---- targets ----------------------------------------------------------------

func (a *App) targetCommands() []cli.Command {
	id := cli.Flag{Name: "id", Type: cli.FlagString, Required: true, Placeholder: "id",
		Description: "the target's id, as \"targets ls\" lists it"}
	return []cli.Command{
		{
			Namespace: "targets", Action: "ls",
			Summary:     "what is in scope",
			Description: "A target decides whether traffic is collected at all. The rules column counts the path rules narrowing it.",
			Run:         a.cmdTargetsList,
		},
		{
			Namespace: "targets", Action: "add",
			Summary: "add a target",
			Description: "The kind is worked out from the value — a domain, a glob (*.example.com), " +
				"an IP address or a CIDR range — unless --kind says otherwise.",
			Flags: []cli.Flag{
				{Name: "value", Type: cli.FlagString, Required: true, Placeholder: "host",
					Description: "the domain, glob, IP address or range"},
				{Name: "kind", Type: cli.FlagString, Placeholder: "kind",
					Description: "domain, wildcard, regex, ip or cidr"},
				{Name: "name", Type: cli.FlagString, Placeholder: "label", Description: "a label for it"},
				{Name: "notes", Type: cli.FlagString, Placeholder: "text", Description: "notes to keep with it"},
			},
			Run: a.cmdTargetsAdd,
		},
		{
			Namespace: "targets", Action: "rm",
			Summary: "remove a target", Flags: []cli.Flag{id},
			Description: "The traffic already collected under it is kept.",
			Run:         a.cmdTargetsRemove,
		},
		{
			Namespace: "targets", Action: "enable",
			Summary: "switch a target on", Flags: []cli.Flag{id}, Run: a.cmdTargetsEnable,
		},
		{
			Namespace: "targets", Action: "disable",
			Summary:     "park a target without deleting it",
			Description: "A disabled target matches nothing, so its traffic stops being collected.",
			Flags:       []cli.Flag{id}, Run: a.cmdTargetsDisable,
		},
		{
			Namespace: "targets", Action: "intercept",
			Summary:     "what this target holds when interception is on",
			Description: "With no flags it reports what the target holds now. DNS holds the target's name lookups as well.",
			Flags: []cli.Flag{
				id,
				{Name: "requests", Type: cli.FlagString, Placeholder: "on|off", Description: "hold its requests"},
				{Name: "responses", Type: cli.FlagString, Placeholder: "on|off", Description: "hold its responses"},
				{Name: "dns", Type: cli.FlagString, Placeholder: "on|off", Description: "hold its name lookups too"},
			},
			Run: a.cmdTargetsIntercept,
		},
		{
			Namespace: "targets", Action: "import",
			Summary: "add the targets in a Burp or HackerOne scope file",
			Description: "Give an absolute path, or \"-\" to read the file from the pipe. " +
				"Entries that can't be imported are reported rather than dropped, and hosts already in the list are skipped.",
			Flags: []cli.Flag{
				{Name: "file", Type: cli.FlagString, Required: true, Placeholder: "path",
					Description: "the scope file, or \"-\" for the piped input"},
				yesFlag("an import can add hundreds of targets"),
			},
			Run: a.cmdTargetsImport,
		},
		{
			Namespace: "targets", Action: "suppressed",
			Summary:     "the hosts that are never offered as related",
			Description: "A suppressed host keeps everything already known about it; it is just never suggested.",
			Run:         a.cmdTargetsSuppressed,
		},
		{
			Namespace: "targets", Action: "suppress",
			Summary: "never offer these hosts as related",
			Flags: []cli.Flag{
				{Name: "host", Type: cli.FlagList, Required: true, Placeholder: "a,b",
					Description: "host patterns, written like a target's value"},
				{Name: "engagement", Type: cli.FlagBool,
					Description: "only in the open engagement, rather than everywhere"},
			},
			Run: a.cmdTargetsSuppress,
		},
		{
			Namespace: "targets", Action: "unsuppress",
			Summary: "offer these hosts again",
			Flags: []cli.Flag{{Name: "host", Type: cli.FlagList, Required: true, Placeholder: "a,b",
				Description: "the patterns to remove, exactly as \"targets suppressed\" lists them"}},
			Run: a.cmdTargetsUnsuppress,
		},
	}
}

func (a *App) cmdTargetsList(_ context.Context, _ *cli.Input) (cli.Result, error) {
	list := a.targets.List()
	rows := make([][]string, 0, len(list))
	for _, t := range list {
		state := "enabled"
		if !t.Enabled {
			state = "disabled"
		}
		rows = append(rows, []string{
			t.ID, string(t.Kind), t.Value, t.Name, state,
			fmt.Sprint(len(t.Rules)), interceptWord(t.Intercept),
		})
	}
	return listing([]string{"ID", "KIND", "VALUE", "NAME", "STATE", "RULES", "HOLDS"}, rows,
		"Nothing is in scope. Without a target, traffic is seen but not collected.", list)
}

// interceptWord says what a target holds, in one word per direction.
func interceptWord(ic target.Intercept) string {
	var parts []string
	if ic.Requests {
		parts = append(parts, "requests")
	}
	if ic.Responses {
		parts = append(parts, "responses")
	}
	if ic.DNS {
		parts = append(parts, "dns")
	}
	if len(parts) == 0 {
		return "nothing"
	}
	return strings.Join(parts, "+")
}

func (a *App) cmdTargetsAdd(_ context.Context, in *cli.Input) (cli.Result, error) {
	kind := target.Kind(strings.ToLower(in.String("kind")))
	t, err := a.targets.Create(kind, in.String("value"))
	if err != nil {
		return cli.Result{}, err
	}
	if name, notes := in.String("name"), in.String("notes"); name != "" || notes != "" {
		t.Name, t.Notes = name, notes
		if err := a.targets.Save(t); err != nil {
			return cli.Result{}, err
		}
	}
	return cli.Data(fmt.Sprintf("Added %s %s (%s).\n", t.Kind, t.Value, t.ID), t)
}

// target reads the target a command names.
func (a *App) target(in *cli.Input) (target.Target, error) {
	t, ok := a.targets.Get(in.String("id"))
	if !ok {
		return target.Target{}, fmt.Errorf("no target %q; \"reproxy targets ls\" lists them", in.String("id"))
	}
	return t, nil
}

func (a *App) cmdTargetsRemove(_ context.Context, in *cli.Input) (cli.Result, error) {
	t, err := a.target(in)
	if err != nil {
		return cli.Result{}, err
	}
	if err := a.targets.Remove(t.ID); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Removed %s. The traffic collected under it is kept.\n", t.Display()), nil
}

func (a *App) setTargetEnabled(in *cli.Input, on bool) (cli.Result, error) {
	t, err := a.target(in)
	if err != nil {
		return cli.Result{}, err
	}
	t.Enabled = on
	if err := a.targets.Save(t); err != nil {
		return cli.Result{}, err
	}
	state := "disabled"
	if on {
		state = "enabled"
	}
	return cli.Data(fmt.Sprintf("%s is %s.\n", t.Display(), state), t)
}

func (a *App) cmdTargetsEnable(_ context.Context, in *cli.Input) (cli.Result, error) {
	return a.setTargetEnabled(in, true)
}

func (a *App) cmdTargetsDisable(_ context.Context, in *cli.Input) (cli.Result, error) {
	return a.setTargetEnabled(in, false)
}

func (a *App) cmdTargetsIntercept(_ context.Context, in *cli.Input) (cli.Result, error) {
	t, err := a.target(in)
	if err != nil {
		return cli.Result{}, err
	}
	changed := false
	for _, f := range []struct {
		flag string
		to   *bool
	}{{"requests", &t.Intercept.Requests}, {"responses", &t.Intercept.Responses}, {"dns", &t.Intercept.DNS}} {
		if !in.Given(f.flag) {
			continue
		}
		on, err := onOff(in.String(f.flag))
		if err != nil {
			return cli.Result{}, fmt.Errorf("--%s: %w", f.flag, err)
		}
		*f.to, changed = on, true
	}
	if changed {
		if err := a.targets.Save(t); err != nil {
			return cli.Result{}, err
		}
	}
	return cli.Data(fmt.Sprintf("%s holds %s.\n", t.Display(), interceptWord(t.Intercept)), t.Intercept)
}

// onOff reads a flag written the way a switch is talked about.
func onOff(s string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "on", "yes", "true", "1":
		return true, nil
	case "off", "no", "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("takes on or off, not %q", s)
}

func (a *App) cmdTargetsImport(_ context.Context, in *cli.Input) (cli.Result, error) {
	r, closer, err := openInput(in, "file")
	if err != nil {
		return cli.Result{}, err
	}
	defer closer()
	imp, err := target.ParseBurpScope(r)
	if err != nil {
		return cli.Result{}, err
	}
	if len(imp.Targets) == 0 {
		return cli.Result{}, fmt.Errorf("no target could be made from that file")
	}
	if err := confirmed(in, fmt.Sprintf("that file holds %s", count(len(imp.Targets), "target", "targets"))); err != nil {
		var preview []string
		for _, t := range imp.Targets {
			preview = append(preview, "  "+t.Display())
		}
		return cli.Result{}, fmt.Errorf("%w:\n%s", err, strings.Join(preview, "\n"))
	}
	added, duplicates, err := a.targets.AddAll(imp.Targets)
	if err != nil {
		return cli.Result{}, err
	}
	said := []string{fmt.Sprintf("Added %s.", count(len(added), "target", "targets"))}
	if duplicates > 0 {
		said = append(said, fmt.Sprintf("%s already in the list.", count(duplicates, "host was", "hosts were")))
	}
	for _, note := range imp.Notes {
		said = append(said, note)
	}
	for _, s := range imp.Skipped {
		said = append(said, fmt.Sprintf("skipped %s: %s", s.Entry, s.Reason))
	}
	return cli.Data(lines(said...), map[string]any{
		"added": added, "duplicates": duplicates, "notes": imp.Notes, "skipped": imp.Skipped,
	})
}

func (a *App) cmdTargetsSuppressed(_ context.Context, _ *cli.Input) (cli.Result, error) {
	list := a.Effective().Targets.Suppressed
	rows := make([][]string, 0, len(list))
	for _, s := range list {
		rows = append(rows, []string{s.Pattern, string(s.Kind()), s.Note, since(s.Added)})
	}
	return listing([]string{"PATTERN", "KIND", "NOTE", "AGE"}, rows,
		"No host is suppressed.", list)
}

func (a *App) cmdTargetsSuppress(_ context.Context, in *cli.Input) (cli.Result, error) {
	hosts := in.List("host")
	if err := a.SuppressHosts(hosts, in.Bool("engagement")); err != nil {
		return cli.Result{}, err
	}
	where := "everywhere"
	if in.Bool("engagement") {
		where = "in this engagement"
	}
	return cli.Text("Suppressed %s %s.\n", count(len(hosts), "host", "hosts"), where), nil
}

func (a *App) cmdTargetsUnsuppress(_ context.Context, in *cli.Input) (cli.Result, error) {
	drop := map[string]bool{}
	for _, h := range in.List("host") {
		drop[strings.ToLower(strings.TrimSpace(h))] = true
	}
	next := a.Settings()
	kept := make([]target.Suppression, 0, len(next.Targets.Suppressed))
	removed := 0
	for _, s := range next.Targets.Suppressed {
		if drop[strings.ToLower(s.Pattern)] {
			removed++
			continue
		}
		kept = append(kept, s)
	}
	if removed == 0 {
		return cli.Result{}, fmt.Errorf("none of those patterns is suppressed; \"reproxy targets suppressed\" lists them")
	}
	next.Targets.Suppressed = kept
	if err := a.ApplySettings(next); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Removed %s from the suppression list.\n", count(removed, "pattern", "patterns")), nil
}

// ---- engagements ------------------------------------------------------------

func (a *App) engagementCommands() []cli.Command {
	dir := cli.Flag{Name: "dir", Type: cli.FlagString, Required: true, Placeholder: "path",
		Description: "the engagement's folder"}
	return []cli.Command{
		{
			Namespace: "engagements", Action: "ls",
			Summary:     "the engagements this instance has opened",
			Description: "An engagement is a folder holding its targets, its traffic and its findings, so it can be copied and reopened.",
			Run:         a.cmdEngagementsList,
		},
		{
			Namespace: "engagements", Action: "current",
			Summary: "which engagement is open", Run: a.cmdEngagementsCurrent,
		},
		{
			Namespace: "engagements", Action: "new",
			Summary: "create an engagement and open it",
			Flags: []cli.Flag{
				{Name: "name", Type: cli.FlagString, Required: true, Placeholder: "name", Description: "what to call it"},
				{Name: "dir", Type: cli.FlagString, Placeholder: "path",
					Description: "where to put it (default: a folder named after it under the data directory)"},
				{Name: "description", Type: cli.FlagString, Placeholder: "text", Description: "what the work is"},
			},
			Run: a.cmdEngagementsNew,
		},
		{
			Namespace: "engagements", Action: "open",
			Summary:     "open an engagement",
			Description: "Its targets, traffic, findings and Explorer come with it, and the one open now is closed first.",
			Flags:       []cli.Flag{dir}, Run: a.cmdEngagementsOpen,
		},
		{
			Namespace: "engagements", Action: "close",
			Summary: "close the open engagement", Run: a.cmdEngagementsClose,
		},
		{
			Namespace: "engagements", Action: "rename",
			Summary: "rename the open engagement",
			Flags: []cli.Flag{
				{Name: "name", Type: cli.FlagString, Placeholder: "name", Description: "the new name"},
				{Name: "description", Type: cli.FlagString, Placeholder: "text", Description: "the new description"},
			},
			Run: a.cmdEngagementsRename,
		},
		{
			Namespace: "engagements", Action: "info",
			Summary:     "what the open engagement holds",
			Description: "The archive is the engagement's capture.db: every flow, finding and Explorer row.",
			Run:         a.cmdEngagementsInfo,
		},
		{
			Namespace: "engagements", Action: "export",
			Summary: "copy the open engagement's archive to a file",
			Description: "A consistent copy is written even while traffic is being recorded, and it opens in anything " +
				"that reads SQLite. Give an absolute path.",
			Flags: []cli.Flag{{Name: "to", Type: cli.FlagString, Required: true, Placeholder: "path",
				Description: "where to write it (it must not exist)"}},
			Run: a.cmdEngagementsExport,
		},
		{
			Namespace: "engagements", Action: "forget",
			Summary:     "remove an engagement from the recent list",
			Description: "Its folder is left alone; this only forgets where it was.",
			Flags:       []cli.Flag{dir}, Run: a.cmdEngagementsForget,
		},
	}
}

func (a *App) cmdEngagementsList(_ context.Context, _ *cli.Input) (cli.Result, error) {
	recent := a.engagements.Recent()
	current, open := a.engagements.Current()
	rows := make([][]string, 0, len(recent))
	for _, dir := range recent {
		mark := ""
		if open && dir == current.Dir {
			mark = "open"
		}
		name := ""
		if e, err := engagement.Open(dir); err == nil {
			name = e.Label()
		}
		rows = append(rows, []string{name, mark, dir})
	}
	return listing([]string{"NAME", "", "FOLDER"}, rows,
		"No engagement has been opened on this instance.", recent)
}

func (a *App) cmdEngagementsCurrent(_ context.Context, _ *cli.Input) (cli.Result, error) {
	e, ok := a.engagements.Current()
	if !ok {
		return cli.Data("No engagement is open. Traffic is kept in the data directory's own archive.\n", nil)
	}
	return cli.Data(lines(
		e.Label(),
		e.Description,
		e.Dir,
	), e)
}

func (a *App) cmdEngagementsNew(_ context.Context, in *cli.Input) (cli.Result, error) {
	dir := in.String("dir")
	if dir == "" {
		dir = engagement.DefaultDir(a.DataDir(), in.String("name"))
	}
	e, err := a.engagements.Create(dir, in.String("name"), in.String("description"), nil)
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Data(fmt.Sprintf("Created and opened %s in %s.\n", e.Label(), e.Dir), e)
}

func (a *App) cmdEngagementsOpen(_ context.Context, in *cli.Input) (cli.Result, error) {
	e, err := a.engagements.Open(in.String("dir"))
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Data(fmt.Sprintf("Opened %s (%s targets).\n", e.Label(), fmt.Sprint(len(a.targets.List()))), e)
}

func (a *App) cmdEngagementsClose(_ context.Context, _ *cli.Input) (cli.Result, error) {
	e, ok := a.engagements.Current()
	if !ok {
		return cli.Text("No engagement is open.\n"), nil
	}
	if err := a.engagements.Close(); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Closed %s.\n", e.Label()), nil
}

func (a *App) cmdEngagementsRename(_ context.Context, in *cli.Input) (cli.Result, error) {
	e, ok := a.engagements.Current()
	if !ok {
		return cli.Result{}, fmt.Errorf("no engagement is open")
	}
	name, desc := in.String("name"), in.String("description")
	if !in.Given("name") {
		name = e.Name
	}
	if !in.Given("description") {
		desc = e.Description
	}
	if err := a.engagements.Rename(name, desc); err != nil {
		return cli.Result{}, err
	}
	now, _ := a.engagements.Current()
	return cli.Data(fmt.Sprintf("Renamed to %s.\n", now.Label()), now)
}

func (a *App) cmdEngagementsInfo(_ context.Context, _ *cli.Input) (cli.Result, error) {
	e, ok := a.engagements.Current()
	if !ok {
		return cli.Result{}, fmt.Errorf("no engagement is open")
	}
	info := map[string]any{
		"name": e.Label(), "dir": e.Dir, "description": e.Description,
		"flows": a.store.Len(), "findings": a.findings.Len(),
		"entities": a.entities.Len(), "targets": len(a.targets.List()),
	}
	return cli.Data(lines(
		e.Label(),
		e.Dir,
		fmt.Sprintf("%s, %s, %s, %s",
			count(a.store.Len(), "flow", "flows"),
			count(a.findings.Len(), "finding", "findings"),
			count(a.entities.Len(), "entity", "entities"),
			count(len(a.targets.List()), "target", "targets")),
	), info)
}

func (a *App) cmdEngagementsExport(_ context.Context, in *cli.Input) (cli.Result, error) {
	if _, ok := a.engagements.Current(); !ok {
		return cli.Result{}, fmt.Errorf("no engagement is open, so there is no archive to export")
	}
	ar := a.recorder.Current()
	if ar == nil {
		return cli.Result{}, fmt.Errorf("this instance is not recording to an archive")
	}
	path := in.String("to")
	size, err := ar.ExportTo(path)
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Wrote %s (%s).\n", path, capture.FormatBytes(size)), nil
}

func (a *App) cmdEngagementsForget(_ context.Context, in *cli.Input) (cli.Result, error) {
	a.engagements.Forget(in.String("dir"))
	return cli.Text("Forgotten. The folder is untouched.\n"), nil
}

// ---- crawl ------------------------------------------------------------------

func (a *App) crawlCommands() []cli.Command {
	return []cli.Command{
		{
			Namespace: "crawl", Action: "status",
			Summary: "what Autocrawl is doing", Run: a.cmdCrawlStatus,
		},
		{
			Namespace: "crawl", Action: "start",
			Summary: "crawl every URL the targets cover",
			Description: "A crawl is noisy and can stress a server, so it asks first. It follows what it is led to — " +
				"the URLs the Explorer knows, robots.txt and sitemaps, and whatever those turn up.",
			Flags: []cli.Flag{
				{Name: "rate", Type: cli.FlagNumber, Placeholder: "n", Description: "requests a second (default: the saved setting)"},
				{Name: "threads", Type: cli.FlagNumber, Placeholder: "n", Description: "requests in flight at once"},
				{Name: "new-only", Type: cli.FlagString, Placeholder: "on|off", Description: "skip URLs earlier runs crawled"},
				{Name: "api-docs", Type: cli.FlagString, Placeholder: "on|off", Description: "guess at the usual OpenAPI locations"},
				userAgentFlag,
				yesFlag("a crawl is noisy and can stress a server"),
			},
			Run: a.cmdCrawlStart,
		},
		{
			Namespace: "crawl", Action: "stop",
			Summary:     "stop crawling",
			Description: "What is queued stays queued, so starting again carries on.",
			Run:         a.cmdCrawlStop,
		},
		{
			Namespace: "crawl", Action: "settings",
			Summary:     "the crawler's settings, and how to change them",
			Description: "With no flags it prints them. They are kept with the engagement.",
			Flags: []cli.Flag{
				{Name: "rate", Type: cli.FlagNumber, Placeholder: "n", Description: "requests a second"},
				{Name: "threads", Type: cli.FlagNumber, Placeholder: "n", Description: "requests in flight at once"},
				{Name: "new-only", Type: cli.FlagString, Placeholder: "on|off", Description: "only crawl URLs earlier runs didn't"},
				{Name: "api-docs", Type: cli.FlagString, Placeholder: "on|off", Description: "guess at the usual OpenAPI locations"},
				userAgentFlag,
			},
			Run: a.cmdCrawlSettings,
		},
		{
			Namespace: "crawl", Action: "recent",
			Summary: "the URLs the crawler fetched most recently",
			Flags:   []cli.Flag{limitFlag("50")},
			Run:     a.cmdCrawlRecent,
		},
	}
}

func (a *App) cmdCrawlStatus(_ context.Context, _ *cli.Input) (cli.Result, error) {
	st := a.crawler.Status()
	state := "idle"
	switch {
	case st.Stopping:
		state = "stopping"
	case st.Running:
		state = "running"
	}
	said := []string{fmt.Sprintf("Autocrawl is %s.", state),
		fmt.Sprintf("%d queued, %d crawled, %d failed, %d waiting for a target, %d in flight.",
			st.Queued, st.Crawled, st.Failed, st.Waiting, st.InFlight)}
	if !st.Recording {
		said = append(said, "No engagement is open, so what it does is forgotten when one is.")
	}
	return cli.Data(lines(said...), st)
}

// userAgentFlag chooses the crawl's User-Agent: one of crawl.UserAgents by
// its ID, or any other text as the header itself.
var userAgentFlag = cli.Flag{Name: "user-agent", Type: cli.FlagString, Placeholder: "id|text",
	Description: "the User-Agent sent: " + userAgentIDs() + ", or the header's text"}

func userAgentIDs() string {
	ids := make([]string, len(crawl.UserAgents))
	for i, ua := range crawl.UserAgents {
		ids[i] = ua.ID
	}
	return strings.Join(ids, ", ")
}

// userAgentName is how a User-Agent is listed: the ID of an offered one,
// or the text of the user's own.
func userAgentName(value string) string {
	for _, ua := range crawl.UserAgents {
		if ua.Value == value {
			return fmt.Sprintf("%s (%s)", ua.ID, ua.Name)
		}
	}
	return value
}

// crawlFlags applies the settings flags a command was given to s.
func crawlFlags(in *cli.Input, s crawl.Settings) (crawl.Settings, error) {
	if in.Given("rate") {
		s.RequestsPerSecond = in.Float("rate")
	}
	if in.Given("threads") {
		s.Threads = in.Int("threads")
	}
	for _, f := range []struct {
		flag string
		to   *bool
	}{{"new-only", &s.OnlyNew}, {"api-docs", &s.APIDocs}} {
		if !in.Given(f.flag) {
			continue
		}
		on, err := onOff(in.String(f.flag))
		if err != nil {
			return s, fmt.Errorf("--%s: %w", f.flag, err)
		}
		*f.to = on
	}
	if in.Given("user-agent") {
		s.UserAgent = in.String("user-agent")
		if ua, ok := crawl.UserAgentByID(s.UserAgent); ok {
			s.UserAgent = ua.Value
		}
	}
	return s, s.Validate()
}

func (a *App) cmdCrawlStart(_ context.Context, in *cli.Input) (cli.Result, error) {
	if err := confirmed(in, "a crawl requests every URL in scope, which is noisy and can stress a server"); err != nil {
		return cli.Result{}, err
	}
	s, err := crawlFlags(in, a.crawler.Settings())
	if err != nil {
		return cli.Result{}, err
	}
	if err := a.crawler.Start(s); err != nil {
		return cli.Result{}, err
	}
	st := a.crawler.Status()
	return cli.Data(fmt.Sprintf("Crawling: %d queued at %g requests a second, %d at a time.\n",
		st.Queued, s.RequestsPerSecond, s.Threads), st)
}

func (a *App) cmdCrawlStop(_ context.Context, _ *cli.Input) (cli.Result, error) {
	a.crawler.Stop()
	st := a.crawler.Status()
	return cli.Data(fmt.Sprintf("Stopped. %d still queued.\n", st.Queued), st)
}

func (a *App) cmdCrawlSettings(_ context.Context, in *cli.Input) (cli.Result, error) {
	s, err := crawlFlags(in, a.crawler.Settings())
	if err != nil {
		return cli.Result{}, err
	}
	changed := in.Given("rate") || in.Given("threads") || in.Given("new-only") || in.Given("api-docs") || in.Given("user-agent")
	if changed {
		if err := a.crawler.SaveSettings(s); err != nil {
			return cli.Result{}, err
		}
	}
	return cli.Data(lines(
		fmt.Sprintf("rate      %g requests a second", s.RequestsPerSecond),
		fmt.Sprintf("threads   %d (at most %d here)", s.Threads, crawl.MaxThreads()),
		fmt.Sprintf("new-only  %s", onOffWord(s.OnlyNew)),
		fmt.Sprintf("api-docs  %s", onOffWord(s.APIDocs)),
		fmt.Sprintf("agent     %s", userAgentName(s.UserAgent)),
		fmt.Sprintf("excluded  %s", count(len(s.ExcludedTargets), "target", "targets")),
	), s)
}

func onOffWord(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func (a *App) cmdCrawlRecent(_ context.Context, in *cli.Input) (cli.Result, error) {
	recent := a.crawler.Recent()
	sort.Slice(recent, func(i, j int) bool { return recent[i].At.After(recent[j].At) })
	if limit := in.Int("limit"); limit > 0 && len(recent) > limit {
		recent = recent[:limit]
	}
	rows := make([][]string, 0, len(recent))
	for _, r := range recent {
		status := fmt.Sprint(r.Status)
		if r.Error != "" {
			status = "failed"
		}
		rows = append(rows, []string{status, since(r.At), elide.Runes(r.URL, 80)})
	}
	return listing([]string{"STATUS", "AGE", "URL"}, rows, "The crawler has fetched nothing yet.", recent)
}
