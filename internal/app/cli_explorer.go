package app

import (
	"context"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"

	"reproxy/internal/cli"
	"reproxy/internal/elide"
	"reproxy/internal/entities"
)

// The entities namespace: the Entity Explorer, which is what the traffic
// turned up — domains, hosts and URLs — and the related hosts waiting to be
// promoted or suppressed.
//
// It is one store with three kinds of entity in it, so it is one namespace
// with --type rather than three; promoting a related host is the same
// target.Store.CreateAll the Related pane calls, and suppressing one the
// same App.SuppressHosts.

func (a *App) entityCommands() []cli.Command {
	key := cli.Flag{Name: "key", Type: cli.FlagString, Required: true, Placeholder: "key",
		Description: "the entity's key, as \"entities ls\" lists it"}
	return []cli.Command{
		{
			Namespace: "entities", Action: "ls",
			Summary: "what the traffic turned up",
			Description: "An entity nothing reached is \"discovered\": something linked to it, or a plugin added it. " +
				"--related lists the hosts no target covers that a target's traffic referred to, which the Explorer leaves out until they are promoted.",
			Flags: []cli.Flag{
				{Name: "type", Type: cli.FlagString, Placeholder: "t",
					Description: "domain, host or url"},
				{Name: "host", Type: cli.FlagString, Placeholder: "host",
					Description: "keep only entities on this host"},
				{Name: "filter", Type: cli.FlagString, Placeholder: "text",
					Description: "keep the entities whose key matches this text"},
				{Name: "visited", Type: cli.FlagBool,
					Description: "only the ones traffic actually reached"},
				{Name: "related", Type: cli.FlagBool,
					Description: "the related hosts instead: what a target referred to but no target covers"},
				{Name: "target", Type: cli.FlagString, Placeholder: "id",
					Description: "keep only what falls under this target"},
				limitFlag("100"),
			},
			Run: a.cmdEntitiesList,
		},
		{
			Namespace: "entities", Action: "get",
			Summary: "one entity in full", Flags: []cli.Flag{key},
			Description: "Its attributes, its key/values, what discovered it, and how to list the requests behind it.",
			Run:         a.cmdEntitiesGet,
		},
		{
			Namespace: "entities", Action: "flows",
			Summary:     "the requests behind an entity",
			Description: "The same listing \"flows ls\" gives, narrowed to what this entity was seen in.",
			Flags:       []cli.Flag{key, limitFlag("50")},
			Run:         a.cmdEntitiesFlows,
		},
		{
			Namespace: "entities", Action: "kv",
			Summary: "set or remove a key on an entity",
			Description: "The key/value store is the user's own notes on an entity; plugins and agents write to the same one. " +
				"With no --value the key is removed; with neither, the entity's keys are listed.",
			Flags: []cli.Flag{
				key,
				{Name: "name", Type: cli.FlagString, Placeholder: "name", Description: "the key"},
				{Name: "value", Type: cli.FlagString, Placeholder: "value", Description: "the value to set"},
			},
			Run: a.cmdEntitiesKV,
		},
		{
			Namespace: "entities", Action: "add",
			Summary:     "put a URL or a host in the Explorer without any traffic",
			Description: "What a lookup or a guess turned up, so it is on the map before anything is sent to it.",
			Flags: []cli.Flag{
				{Name: "url", Type: cli.FlagString, Placeholder: "url", Description: "the URL to add"},
				{Name: "host", Type: cli.FlagString, Placeholder: "host", Description: "the host to add instead"},
				{Name: "address", Type: cli.FlagList, Placeholder: "a,b", Description: "addresses the host resolves to"},
				{Name: "note", Type: cli.FlagString, Placeholder: "text", Default: "cli",
					Description: "what to record as having found it"},
			},
			Run: a.cmdEntitiesAdd,
		},
		{
			Namespace: "entities", Action: "promote",
			Summary:     "make related hosts targets",
			Description: "The same thing the Targets section's Related pane does: one write, and the crawler picks them up at once.",
			Flags: []cli.Flag{{Name: "host", Type: cli.FlagList, Required: true, Placeholder: "a,b",
				Description: "the hosts to promote, as \"entities ls --related\" lists them"}},
			Run: a.cmdEntitiesPromote,
		},
		{
			Namespace: "entities", Action: "suppress",
			Summary:     "stop offering related hosts",
			Description: "A shorthand for \"targets suppress\": what is known about the host is kept, it is just never suggested again.",
			Flags: []cli.Flag{
				{Name: "host", Type: cli.FlagList, Required: true, Placeholder: "a,b", Description: "the hosts to suppress"},
				{Name: "engagement", Type: cli.FlagBool, Description: "only in the open engagement"},
			},
			Run: a.cmdTargetsSuppress,
		},
		{
			Namespace: "entities", Action: "rm",
			Summary: "delete an entity and everything under it", Flags: []cli.Flag{key},
			Run: a.cmdEntitiesRemove,
		},
		{
			Namespace: "entities", Action: "clear",
			Summary:     "empty the Explorer",
			Description: "The traffic and the findings are kept; the Explorer fills again as requests arrive.",
			Flags:       []cli.Flag{yesFlag("this cannot be undone")},
			Run:         a.cmdEntitiesClear,
		},
	}
}

func (a *App) cmdEntitiesList(_ context.Context, in *cli.Input) (cli.Result, error) {
	if err := a.engagementOpen("entities"); err != nil {
		return cli.Result{}, err
	}
	limit := in.Int("limit")
	if in.Bool("related") {
		related := a.entities.Related()
		if len(related) > limit {
			related = related[:limit]
		}
		rows := make([][]string, 0, len(related))
		for _, r := range related {
			rows = append(rows, []string{
				r.Host, strconv.Itoa(r.URLs), elide.Runes(strings.Join(r.FoundOn, " "), 50), since(r.LastSeen),
			})
		}
		return listing([]string{"HOST", "URLS", "REFERRED FROM", "LAST SEEN"}, rows,
			"No host is waiting to be promoted.", related)
	}

	want := strings.ToLower(in.String("type"))
	switch want {
	case "", "domain", "host", "url":
	default:
		return cli.Result{}, fmt.Errorf("--type takes domain, host or url, not %q", want)
	}
	list := a.entities.List()
	if id := in.String("target"); id != "" {
		list = a.entities.ByTarget(id)
	}
	filter := strings.ToLower(in.String("filter"))
	host := strings.ToLower(in.String("host"))
	kept := make([]entities.Entity, 0, len(list))
	for _, e := range list {
		switch {
		case want != "" && string(e.Type) != want,
			host != "" && !strings.EqualFold(e.Host, host),
			filter != "" && !strings.Contains(strings.ToLower(e.Key), filter),
			in.Bool("visited") && !e.Visited():
			continue
		}
		kept = append(kept, e)
		if len(kept) >= limit {
			break
		}
	}
	rows := make([][]string, 0, len(kept))
	for _, e := range kept {
		state := "discovered"
		if e.Visited() {
			state = strconv.Itoa(e.Hits) + " hits"
		}
		rows = append(rows, []string{string(e.Type), state, since(e.LastSeen), elide.Runes(e.Key, 80)})
	}
	return listing([]string{"TYPE", "SEEN", "AGE", "KEY"}, rows,
		"The Explorer is empty. It fills as traffic arrives.", kept)
}

// entity reads the entity a command names.
func (a *App) entity(in *cli.Input) (entities.Entity, error) {
	if err := a.engagementOpen("entities"); err != nil {
		return entities.Entity{}, err
	}
	key := in.String("key")
	e, ok := a.entities.Get(key)
	if !ok {
		return entities.Entity{}, fmt.Errorf("no entity %q; \"reproxy entities ls\" lists them", key)
	}
	return e, nil
}

func (a *App) cmdEntitiesGet(_ context.Context, in *cli.Input) (cli.Result, error) {
	e, err := a.entity(in)
	if err != nil {
		return cli.Result{}, err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s %s\n", e.Type, e.Key)
	seen := "discovered, no traffic"
	if e.Visited() {
		seen = fmt.Sprintf("%s, last %s ago", count(e.Hits, "hit", "hits"), since(e.LastSeen))
	}
	fmt.Fprintf(&b, "%s · found by %s\n", seen, e.Origin)
	if e.Parent != "" {
		fmt.Fprintf(&b, "under %s\n", e.Parent)
	}
	switch {
	case e.DomainAttrs != nil:
		fmt.Fprintf(&b, "suffix %s\n", e.DomainAttrs.Suffix)
	case e.HostAttrs != nil && len(e.HostAttrs.Addresses) > 0:
		fmt.Fprintf(&b, "addresses %s\n", strings.Join(e.HostAttrs.Addresses, ", "))
	case e.URLAttrs != nil:
		if ops := e.Operations(); len(ops) > 0 {
			b.WriteString("operations\n")
			for _, op := range ops {
				fmt.Fprintf(&b, "  %s · %s · %s\n", op.Method,
					count(op.Requests, "request", "requests"),
					count(len(op.Query)+len(op.Body), "parameter", "parameters"))
			}
		}
	}
	if len(e.KV) > 0 {
		b.WriteString("keys\n")
		for _, p := range e.KV {
			fmt.Fprintf(&b, "  %s = %s\n", p.Key, p.Value)
		}
	}
	if from := e.LinkedFrom(); len(from) > 0 {
		fmt.Fprintf(&b, "linked from %s\n", elide.Runes(strings.Join(from, ", "), 200))
	}
	if n := len(e.FlowIDs()); n > 0 {
		fmt.Fprintf(&b, "%s: reproxy entities flows --key %s\n", count(n, "request", "requests"), e.Key)
	}
	return cli.Data(b.String(), e)
}

func (a *App) cmdEntitiesFlows(ctx context.Context, in *cli.Input) (cli.Result, error) {
	e, err := a.entity(in)
	if err != nil {
		return cli.Result{}, err
	}
	ids := a.entities.FlowIDs(e.Key)
	if len(ids) == 0 {
		return cli.Data("No request is recorded against that entity.\n", nil)
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
	for _, f := range page.Flows {
		rows = append(rows, []string{
			strconv.FormatUint(f.ID, 10), f.Method, strconv.Itoa(f.Status), since(f.Started), elide.Runes(f.URL, 70),
		})
	}
	return listing([]string{"ID", "METHOD", "STATUS", "AGE", "URL"}, rows,
		"The requests behind that entity are no longer in the list.", page)
}

func (a *App) cmdEntitiesKV(_ context.Context, in *cli.Input) (cli.Result, error) {
	e, err := a.entity(in)
	if err != nil {
		return cli.Result{}, err
	}
	name := in.String("name")
	if name == "" {
		rows := make([][]string, 0, len(e.KV))
		for _, p := range e.KV {
			rows = append(rows, []string{p.Key, p.Value})
		}
		return listing([]string{"KEY", "VALUE"}, rows, "That entity has no keys.", e.KV)
	}
	if !in.Given("value") {
		next, err := a.entities.DeleteKV(e.Key, name)
		if err != nil {
			return cli.Result{}, err
		}
		return cli.Data(fmt.Sprintf("Removed %s from %s.\n", name, e.Key), next.KV)
	}
	next, err := a.entities.SetKV(e.Key, name, in.String("value"))
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Data(fmt.Sprintf("%s = %s on %s\n", name, in.String("value"), e.Key), next.KV)
}

func (a *App) cmdEntitiesAdd(_ context.Context, in *cli.Input) (cli.Result, error) {
	if err := a.engagementOpen("entities"); err != nil {
		return cli.Result{}, err
	}
	rawURL, host := in.String("url"), in.String("host")
	if (rawURL == "") == (host == "") {
		return cli.Result{}, fmt.Errorf("give either --url or --host")
	}
	// The link records what put it there, the way a plugin's does, so the
	// Explorer can always say where something came from.
	link := entities.Link{Source: "cli", Detail: in.String("note")}
	if host != "" {
		e, created, err := a.entities.DiscoverHost(entities.HostDiscovery{
			Name: host, Addresses: in.List("address"), Link: link,
			TargetID: a.targetOf("http://" + host),
		})
		if err != nil {
			return cli.Result{}, err
		}
		return cli.Data(fmt.Sprintf("%s %s\n", addedWord(created), e.Key), e)
	}
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return cli.Result{}, fmt.Errorf("--url takes an absolute http or https URL, not %q", rawURL)
	}
	e, created, err := a.entities.Discover(entities.Discovery{
		Address: entities.AddressOf(u.Scheme, u.Host, u.Path), Link: link, TargetID: a.targetOf(rawURL),
	})
	if err != nil {
		return cli.Result{}, err
	}
	return cli.Data(fmt.Sprintf("%s %s\n", addedWord(created), e.Key), e)
}

func addedWord(created bool) string {
	if created {
		return "Added"
	}
	return "Already known:"
}

func (a *App) cmdEntitiesPromote(_ context.Context, in *cli.Input) (cli.Result, error) {
	hosts := in.List("host")
	added, err := a.targets.CreateAll("", hosts)
	if err != nil {
		return cli.Result{}, err
	}
	names := make([]string, 0, len(added))
	for _, t := range added {
		names = append(names, t.Display())
	}
	sort.Strings(names)
	return cli.Data(fmt.Sprintf("Promoted %s: %s\n",
		count(len(added), "host", "hosts"), strings.Join(names, ", ")), added)
}

func (a *App) cmdEntitiesRemove(_ context.Context, in *cli.Input) (cli.Result, error) {
	e, err := a.entity(in)
	if err != nil {
		return cli.Result{}, err
	}
	if err := a.entities.Delete(e.Key); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Deleted %s and everything under it.\n", e.Key), nil
}

func (a *App) cmdEntitiesClear(_ context.Context, in *cli.Input) (cli.Result, error) {
	if err := a.engagementOpen("entities"); err != nil {
		return cli.Result{}, err
	}
	n := a.entities.Len()
	if n == 0 {
		return cli.Text("The Explorer is already empty.\n"), nil
	}
	if err := confirmed(in, fmt.Sprintf("this removes %s", count(n, "entity", "entities"))); err != nil {
		return cli.Result{}, err
	}
	if err := a.entities.Clear(); err != nil {
		return cli.Result{}, err
	}
	return cli.Text("Emptied the Explorer (%s). The traffic and the findings are kept.\n",
		count(n, "entity", "entities")), nil
}
