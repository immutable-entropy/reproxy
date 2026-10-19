package app

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"reproxy/internal/cli"
	"reproxy/internal/elide"
	"reproxy/internal/findings"
)

// The findings namespace: what the work turned up. Every route into the
// store — the user's right-click, an agent's create_finding, a plugin's
// findings.record and these commands — goes through findings.Store.Record,
// so a value recorded here merges with the same value recorded anywhere
// else rather than becoming a second finding.

func (a *App) findingCommands() []cli.Command {
	id := cli.Flag{Name: "id", Type: cli.FlagNumber, Required: true, Placeholder: "id",
		Description: "the finding's id, as \"findings ls\" lists it"}
	return []cli.Command{
		{
			Namespace: "findings", Action: "ls",
			Summary: "what has been found",
			Description: "A finding scored below the floor is noise an agent has judged, and is left out unless " +
				"--min-confidence says otherwise. A hidden value is shown masked here as it is in the window.",
			Flags: []cli.Flag{
				{Name: "type", Type: cli.FlagList, Placeholder: "a,b",
					Description: "keep only findings of these types (id or name)"},
				{Name: "unviewed", Type: cli.FlagBool, Description: "only the ones nobody has opened"},
				{Name: "unreviewed", Type: cli.FlagBool, Description: "only the ones nothing has scored yet"},
				{Name: "min-confidence", Type: cli.FlagNumber, Placeholder: "0-100",
					Description: "hide scored findings below this (default: the noise floor)"},
				{Name: "all", Type: cli.FlagBool, Description: "include the ones below the floor"},
				{Name: "filter", Type: cli.FlagString, Placeholder: "text",
					Description: "keep the findings whose value, notes or types match"},
				{Name: "reveal", Type: cli.FlagBool, Description: "show hidden values in full"},
				limitFlag("50"),
			},
			Run: a.cmdFindingsList,
		},
		{
			Namespace: "findings", Action: "get",
			Summary: "one finding, its notes and every request it was seen in",
			Flags:   []cli.Flag{id, {Name: "reveal", Type: cli.FlagBool, Description: "show a hidden value in full"}},
			Run:     a.cmdFindingsGet,
		},
		{
			Namespace: "findings", Action: "new",
			Summary: "record a finding",
			Description: "A finding whose value one already holds is merged into it, taking its types and notes, " +
				"rather than being added twice. Mark a value that is itself a secret with --hidden.",
			Flags: []cli.Flag{
				{Name: "value", Type: cli.FlagString, Placeholder: "text",
					Description: "the thing itself, or \"-\" for the piped input"},
				{Name: "type", Type: cli.FlagList, Placeholder: "a,b",
					Description: "the types classifying it (id or name)"},
				{Name: "notes", Type: cli.FlagString, Placeholder: "text",
					Description: "what it is and why it matters"},
				{Name: "flow", Type: cli.FlagNumber, Placeholder: "id",
					Description: "the request it was seen in"},
				{Name: "location", Type: cli.FlagString, Placeholder: "where",
					Description: "request_header, request_body, response_header or response_body"},
				{Name: "detail", Type: cli.FlagString, Placeholder: "text",
					Description: "the exact spot, e.g. the header's name"},
				{Name: "hidden", Type: cli.FlagBool, Description: "the value is a secret in itself: mask it on screen"},
				{Name: "confidence", Type: cli.FlagNumber, Placeholder: "0-100", Description: "how likely it is a true positive"},
			},
			Run: a.cmdFindingsNew,
		},
		{
			Namespace: "findings", Action: "rm",
			Summary: "delete a finding", Flags: []cli.Flag{id}, Run: a.cmdFindingsRemove,
		},
		{
			Namespace: "findings", Action: "notes",
			Summary: "replace a finding's notes",
			Flags: []cli.Flag{id, {Name: "text", Type: cli.FlagString, Required: true, Placeholder: "text",
				Description: "the notes, or \"-\" for the piped input"}},
			Run: a.cmdFindingsNotes,
		},
		{
			Namespace: "findings", Action: "classify",
			Summary:     "set which types a finding carries",
			Description: "The types replace the ones it has. \"findings types\" lists what the catalog holds.",
			Flags: []cli.Flag{id, {Name: "type", Type: cli.FlagList, Required: true, Placeholder: "a,b",
				Description: "the types (id or name); an empty list leaves it untyped"}},
			Run: a.cmdFindingsClassify,
		},
		{
			Namespace: "findings", Action: "kv",
			Summary:     "set or remove a key on a finding",
			Description: "With no --value the key is removed; with neither, the finding's keys are listed.",
			Flags:       []cli.Flag{id, {Name: "name", Type: cli.FlagString, Placeholder: "name", Description: "the key"}, {Name: "value", Type: cli.FlagString, Placeholder: "value", Description: "the value"}},
			Run:         a.cmdFindingsKV,
		},
		{
			Namespace: "findings", Action: "confidence",
			Summary: "score how likely a finding is a true positive",
			Description: "0 to 100. A finding starts unreviewed, which is deliberately not the same as a " +
				"considered 50; --clear puts it back to unreviewed.",
			Flags: []cli.Flag{
				id,
				{Name: "score", Type: cli.FlagNumber, Placeholder: "0-100", Description: "the score"},
				{Name: "clear", Type: cli.FlagBool, Description: "mark it unreviewed again"},
			},
			Run: a.cmdFindingsConfidence,
		},
		{
			Namespace: "findings", Action: "hide",
			Summary:     "mask a finding's value on screen",
			Description: "For a value that is a secret in itself. It is still stored, searched and given to agents and plugins.",
			Flags:       []cli.Flag{id}, Run: a.cmdFindingsHide,
		},
		{
			Namespace: "findings", Action: "show",
			Summary: "stop masking a finding's value", Flags: []cli.Flag{id}, Run: a.cmdFindingsShow,
		},
		{
			Namespace: "findings", Action: "flows",
			Summary: "the requests a finding was seen in",
			Flags:   []cli.Flag{id, limitFlag("50")},
			Run:     a.cmdFindingsFlows,
		},
		{
			Namespace: "findings", Action: "types",
			Summary:     "the type catalog",
			Description: "The user's types from the settings file, and the types the running plugins declare while they run.",
			Run:         a.cmdFindingsTypes,
		},
		{
			Namespace: "findings", Action: "viewed",
			Summary:     "mark findings as seen",
			Description: "What the Findings badge counts. With no --id, every unviewed finding is marked.",
			Flags: []cli.Flag{{Name: "id", Type: cli.FlagNumber, Placeholder: "id",
				Description: "one finding, rather than all of them"}},
			Run: a.cmdFindingsViewed,
		},
	}
}

// findingValue is how a value is printed: masked unless it is not hidden,
// or the user asked to see it.
func findingValue(f findings.Finding, reveal bool) string {
	if f.Hidden && !reveal {
		return f.Masked()
	}
	return f.Value
}

func (a *App) cmdFindingsList(_ context.Context, in *cli.Input) (cli.Result, error) {
	if err := a.engagementOpen("findings"); err != nil {
		return cli.Result{}, err
	}
	q := findings.Query{
		Unviewed:       in.Bool("unviewed"),
		UnreviewedOnly: in.Bool("unreviewed"),
		MinConfidence:  findings.ConfidenceFloor,
	}
	switch {
	case in.Bool("all"):
		q.MinConfidence = 0
	case in.Given("min-confidence"):
		q.MinConfidence = in.Int("min-confidence")
	}
	for _, name := range in.List("type") {
		t, ok := a.findings.ResolveType(name)
		if !ok {
			return cli.Result{}, fmt.Errorf("no finding type %q; \"reproxy findings types\" lists them", name)
		}
		q.Types = append(q.Types, t.ID)
	}
	list := a.findings.Select(q)

	filter := strings.ToLower(in.String("filter"))
	reveal := in.Bool("reveal")
	limit := in.Int("limit")
	kept := make([]findings.Finding, 0, len(list))
	rows := make([][]string, 0, len(list))
	for _, f := range list {
		if filter != "" && !strings.Contains(strings.ToLower(f.SearchText()), filter) {
			continue
		}
		if len(kept) >= limit {
			break
		}
		kept = append(kept, f)
		rows = append(rows, []string{
			strconv.FormatUint(f.ID, 10), confidenceWord(f.Confidence),
			strings.Join(a.typeNames(f.Types), ","), string(f.Source),
			strconv.Itoa(len(f.Links)), elide.Runes(findingValue(f, reveal), 60),
		})
	}
	return listing([]string{"ID", "SCORE", "TYPES", "SOURCE", "SEEN", "VALUE"}, rows,
		"Nothing has been found yet.", kept)
}

// confidenceWord is a score as a column: a number, or "—" for unreviewed.
func confidenceWord(c *int) string {
	if c == nil {
		return "—"
	}
	return strconv.Itoa(*c)
}

// typeNames turns type IDs into the names the catalog shows.
func (a *App) typeNames(ids []string) []string {
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if t, ok := a.findings.Type(id); ok {
			out = append(out, t.Name)
			continue
		}
		out = append(out, id)
	}
	return out
}

// finding reads the finding a command names.
func (a *App) finding(in *cli.Input) (findings.Finding, error) {
	if err := a.engagementOpen("findings"); err != nil {
		return findings.Finding{}, err
	}
	id := uint64(in.Int("id"))
	f, ok := a.findings.Get(id)
	if !ok {
		return findings.Finding{}, fmt.Errorf("no finding %d; \"reproxy findings ls\" lists them", id)
	}
	return f, nil
}

func (a *App) cmdFindingsGet(_ context.Context, in *cli.Input) (cli.Result, error) {
	f, err := a.finding(in)
	if err != nil {
		return cli.Result{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "#%d %s\n", f.ID, findingValue(f, in.Bool("reveal")))
	about := []string{"raised by " + string(f.Source) + authorSuffix(f), "confidence " + confidenceWord(f.Confidence)}
	if types := a.typeNames(f.Types); len(types) > 0 {
		about = append([]string{strings.Join(types, ", ")}, about...)
	} else {
		about = append([]string{"untyped"}, about...)
	}
	fmt.Fprintf(&b, "%s\n", strings.Join(about, " · "))
	if f.Hidden && !in.Bool("reveal") {
		b.WriteString("the value is masked here: --reveal shows it\n")
	}
	if f.Notes != "" {
		fmt.Fprintf(&b, "\n%s\n", strings.TrimRight(f.Notes, "\n"))
	}
	if len(f.Fields) > 0 {
		b.WriteString("\nkeys\n")
		for _, kv := range f.Fields {
			fmt.Fprintf(&b, "  %s = %s\n", kv.Key, kv.Value)
		}
	}
	if len(f.Links) > 0 {
		b.WriteString("\nseen in\n")
		for _, l := range f.Links {
			// Label already reads "METHOD URL — where (detail)", which is
			// the whole sighting; the flow id is what this adds.
			fmt.Fprintf(&b, "  flow %d: %s\n", l.FlowID, elide.Runes(l.Label(), 100))
		}
	}
	return cli.Data(b.String(), f)
}

func authorSuffix(f findings.Finding) string {
	if f.Author == "" {
		return ""
	}
	return " (" + f.Author + ")"
}

func (a *App) cmdFindingsNew(_ context.Context, in *cli.Input) (cli.Result, error) {
	if err := a.engagementOpen("findings"); err != nil {
		return cli.Result{}, err
	}
	d := findings.Draft{
		Value:  in.Text("value"),
		Types:  in.List("type"),
		Notes:  in.Text("notes"),
		Source: findings.SourceUser,
		Hidden: in.Bool("hidden"),
	}
	if in.Given("confidence") {
		score := in.Int("confidence")
		d.Confidence = &score
	}
	if id := uint64(in.Int("flow")); id > 0 {
		f, ok := a.store.Get(id)
		if !ok {
			return cli.Result{}, fmt.Errorf("no flow %d", id)
		}
		d.Link = findings.Link{
			FlowID: id, Method: f.Method, URL: f.URL(),
			Location: in.String("location"), Detail: in.String("detail"),
		}
	}
	if strings.TrimSpace(d.Value) == "" && strings.TrimSpace(d.Notes) == "" {
		return cli.Result{}, fmt.Errorf("a finding needs a --value, or --notes saying what it is")
	}
	f, created, err := a.findings.Record(d)
	if err != nil {
		return cli.Result{}, err
	}
	what := "Merged into finding"
	if created {
		what = "Recorded finding"
	}
	return cli.Data(fmt.Sprintf("%s #%d.\n", what, f.ID), f)
}

func (a *App) cmdFindingsRemove(_ context.Context, in *cli.Input) (cli.Result, error) {
	f, err := a.finding(in)
	if err != nil {
		return cli.Result{}, err
	}
	if err := a.findings.Remove(f.ID); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Deleted finding #%d.\n", f.ID), nil
}

func (a *App) cmdFindingsNotes(_ context.Context, in *cli.Input) (cli.Result, error) {
	f, err := a.finding(in)
	if err != nil {
		return cli.Result{}, err
	}
	if err := a.findings.SetNotes(f.ID, in.Text("text")); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Set the notes on finding #%d.\n", f.ID), nil
}

func (a *App) cmdFindingsClassify(_ context.Context, in *cli.Input) (cli.Result, error) {
	f, err := a.finding(in)
	if err != nil {
		return cli.Result{}, err
	}
	ids := make([]string, 0, len(in.List("type")))
	for _, name := range in.List("type") {
		t, ok := a.findings.ResolveType(name)
		if !ok {
			return cli.Result{}, fmt.Errorf("no finding type %q; \"reproxy findings types\" lists them", name)
		}
		ids = append(ids, t.ID)
	}
	if err := a.findings.Classify(f.ID, ids); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Finding #%d is %s.\n", f.ID, strings.Join(a.typeNames(ids), ", ")), nil
}

func (a *App) cmdFindingsKV(_ context.Context, in *cli.Input) (cli.Result, error) {
	f, err := a.finding(in)
	if err != nil {
		return cli.Result{}, err
	}
	name := in.String("name")
	if name == "" {
		rows := make([][]string, 0, len(f.Fields))
		for _, kv := range f.Fields {
			rows = append(rows, []string{kv.Key, kv.Value})
		}
		return listing([]string{"KEY", "VALUE"}, rows, "That finding has no keys.", f.Fields)
	}
	next := make([]findings.Field, 0, len(f.Fields)+1)
	replaced := false
	for _, kv := range f.Fields {
		if kv.Key == name {
			replaced = true
			if in.Given("value") {
				next = append(next, findings.Field{Key: name, Value: in.String("value")})
			}
			continue
		}
		next = append(next, kv)
	}
	if !replaced {
		if !in.Given("value") {
			return cli.Result{}, fmt.Errorf("finding #%d has no key %q", f.ID, name)
		}
		next = append(next, findings.Field{Key: name, Value: in.String("value")})
	}
	if err := a.findings.SetFields(f.ID, next); err != nil {
		return cli.Result{}, err
	}
	if !in.Given("value") {
		return cli.Data(fmt.Sprintf("Removed %s from finding #%d.\n", name, f.ID), next)
	}
	return cli.Data(fmt.Sprintf("%s = %s on finding #%d\n", name, in.String("value"), f.ID), next)
}

func (a *App) cmdFindingsConfidence(_ context.Context, in *cli.Input) (cli.Result, error) {
	f, err := a.finding(in)
	if err != nil {
		return cli.Result{}, err
	}
	if in.Bool("clear") {
		if err := a.findings.SetConfidence(f.ID, nil); err != nil {
			return cli.Result{}, err
		}
		return cli.Text("Finding #%d is unreviewed again.\n", f.ID), nil
	}
	if !in.Given("score") {
		return cli.Result{}, fmt.Errorf("give --score 0-100, or --clear to mark it unreviewed")
	}
	score := in.Int("score")
	if score < findings.ConfidenceMin || score > findings.ConfidenceMax {
		return cli.Result{}, fmt.Errorf("--score takes %d to %d", findings.ConfidenceMin, findings.ConfidenceMax)
	}
	if err := a.findings.SetConfidence(f.ID, &score); err != nil {
		return cli.Result{}, err
	}
	note := ""
	if score < findings.ConfidenceFloor {
		note = " (below the floor, so it is left out of listings unless asked for)"
	}
	return cli.Text("Finding #%d scored %d%s.\n", f.ID, score, note), nil
}

func (a *App) setFindingHidden(in *cli.Input, hidden bool) (cli.Result, error) {
	f, err := a.finding(in)
	if err != nil {
		return cli.Result{}, err
	}
	if err := a.findings.SetHidden(f.ID, hidden); err != nil {
		return cli.Result{}, err
	}
	if hidden {
		return cli.Text("Finding #%d is masked on screen.\n", f.ID), nil
	}
	return cli.Text("Finding #%d is shown in full.\n", f.ID), nil
}

func (a *App) cmdFindingsHide(_ context.Context, in *cli.Input) (cli.Result, error) {
	return a.setFindingHidden(in, true)
}

func (a *App) cmdFindingsShow(_ context.Context, in *cli.Input) (cli.Result, error) {
	return a.setFindingHidden(in, false)
}

func (a *App) cmdFindingsFlows(ctx context.Context, in *cli.Input) (cli.Result, error) {
	f, err := a.finding(in)
	if err != nil {
		return cli.Result{}, err
	}
	ids := make([]uint64, 0, len(f.Links))
	for _, l := range f.Links {
		if l.FlowID > 0 {
			ids = append(ids, l.FlowID)
		}
	}
	if len(ids) == 0 {
		return cli.Data("That finding is not pinned to a request.\n", f.Links)
	}
	local, err := a.self()
	if err != nil {
		return cli.Result{}, err
	}
	page, err := local.ListFlows(ctx, flowsByID(ids, in.Int("limit")))
	if err != nil {
		return cli.Result{}, err
	}
	rows := make([][]string, 0, len(page.Flows))
	for _, s := range page.Flows {
		rows = append(rows, []string{
			strconv.FormatUint(s.ID, 10), s.Method, strconv.Itoa(s.Status), elide.Runes(s.URL, 70),
		})
	}
	// Each sighting carries its own snapshot, so a finding still says where
	// it was seen after the traffic list has been cleared.
	if len(rows) == 0 {
		var said []string
		for _, l := range f.Links {
			said = append(said, fmt.Sprintf("  %s %s %s", l.Method, l.URL, l.Label()))
		}
		return cli.Data(lines(append([]string{"Those requests are no longer in the list. It was seen in:"}, said...)...), f.Links)
	}
	return listing([]string{"ID", "METHOD", "STATUS", "URL"}, rows, "", page)
}

func (a *App) cmdFindingsTypes(_ context.Context, _ *cli.Input) (cli.Result, error) {
	types := a.findings.Types()
	rows := make([][]string, 0, len(types))
	for _, t := range types {
		rows = append(rows, []string{t.ID, t.Name, t.Color})
	}
	return listing([]string{"ID", "NAME", "COLOUR"}, rows, "The catalog is empty.", types)
}

func (a *App) cmdFindingsViewed(_ context.Context, in *cli.Input) (cli.Result, error) {
	if err := a.engagementOpen("findings"); err != nil {
		return cli.Result{}, err
	}
	if in.Given("id") {
		f, err := a.finding(in)
		if err != nil {
			return cli.Result{}, err
		}
		if err := a.findings.MarkViewed(f.ID); err != nil {
			return cli.Result{}, err
		}
		return cli.Text("Finding #%d is marked as seen.\n", f.ID), nil
	}
	n, err := a.findings.MarkAllViewed()
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Marked %s as seen.\n", count(n, "finding", "findings")), nil
}
