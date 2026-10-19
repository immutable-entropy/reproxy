package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"reproxy/internal/filter"
	"reproxy/internal/findings"
	"reproxy/internal/instance"
)

// The findings tools: the agent's way of writing down what it concluded,
// and of reading back what is already recorded so it doesn't go over the
// same ground.
//
// Findings belong to the open engagement, which is one process's state, so
// unlike the flow and Replay tools these act on the local store and take no
// instance argument. A flow handle passed to create_finding must therefore
// name this instance.
//
// create_finding is a Writes tool, not a Mutates one: it changes nothing
// outside this machine, so it never interrupts the user for approval — but a
// read-only client may not call it. Every finding raised this way must carry
// a note; findings.Draft enforces that, and the error goes back to the model
// so it can call again with one.

// findingOut is a finding in tool output.
type findingOut struct {
	Handle     string           `json:"handle"`
	Value      string           `json:"value,omitempty"`
	Types      []string         `json:"types,omitempty"`
	Notes      string           `json:"notes,omitempty"`
	Fields     []findings.Field `json:"fields,omitempty"`
	Source     string           `json:"source"`
	Author     string           `json:"author,omitempty"`
	Viewed     bool             `json:"viewed"`
	Hidden     bool             `json:"hidden,omitempty"`
	Confidence *int             `json:"confidence"`
	Concern    *int             `json:"concern"`
	Seen       []seenOut        `json:"seen,omitempty"`
	Created    time.Time        `json:"created"`
	Updated    time.Time        `json:"updated"`
}

// seenOut is one sighting: where the finding was seen, and the flow it was
// in. The flow handle may no longer resolve — a finding outlives the traffic
// it came from — which is why the method and URL are repeated here.
type seenOut struct {
	Flow     string `json:"flow,omitempty"`
	Method   string `json:"method,omitempty"`
	URL      string `json:"url,omitempty"`
	Location string `json:"location,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

func toFindingOut(instanceID string, f findings.Finding) findingOut {
	out := findingOut{
		Handle:     findingHandle(instanceID, f.ID),
		Value:      capped(f.Value),
		Types:      f.Types,
		Notes:      capped(f.Notes),
		Fields:     f.Fields,
		Source:     string(f.Source),
		Author:     f.Author,
		Viewed:     f.Viewed,
		Hidden:     f.Hidden,
		Confidence: f.Confidence,
		Concern:    f.Concern,
		Created:    f.Created,
		Updated:    f.Updated,
	}
	for _, l := range f.Links {
		s := seenOut{Method: l.Method, URL: l.URL, Location: l.Location, Detail: l.Detail}
		if l.FlowID != 0 {
			s.Flow = flowHandle(instanceID, l.FlowID)
		}
		out.Seen = append(out.Seen, s)
	}
	return out
}

var listFindingTypes = &Tool{
	Name: "list_finding_types",
	Description: "List the finding types a finding can be classified with. They are the user's own " +
		"taxonomy, configured in Settings → Findings, so read them before recording a finding and " +
		"use the ids you get back.",
	Schema: object(nil),
	run: func(_ context.Context, r *Registry, _ Caller, _ json.RawMessage) (any, error) {
		notes, err := r.notes()
		if err != nil {
			return nil, err
		}
		types := notes.Types()
		out := make([]findings.Type, 0, len(types))
		out = append(out, types...)
		return map[string]any{"types": out}, nil
	},
}

var listFindings = &Tool{
	Name: "list_findings",
	Description: "List the findings recorded in this instance's engagement, newest first. Use it to see " +
		"what has already been found — recording the same value again only adds a sighting to the " +
		"finding that holds it.",
	Schema: object(map[string]any{
		"types":          map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "Keep only findings carrying at least one of these type ids."},
		"unviewed":       boolField("Keep only the findings the user hasn't opened yet."),
		"min_confidence": intField("Hide reviewed findings scored below this (0–100; see confidence on each finding). Unreviewed findings (confidence null) are always kept, since they haven't been judged. 0 (the default) keeps everything, including the noise already scored down. The user's list hides anything below 40 by default."),
		"unreviewed":     boolField("Keep only findings that have not been scored yet (confidence is null). Use it to find what still needs review."),
		"query":          strField("Keep only findings whose value, notes, fields or sightings match this text."),
		"regex":          boolField("Treat query as a regular expression rather than a substring."),
		"flow":           strField("Keep only findings seen in this flow (a flow handle from list_flows)."),
		"limit":          intField("The most findings to return; the default is 50."),
	}),
	run: func(_ context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Types         []string `json:"types"`
			Unviewed      bool     `json:"unviewed"`
			MinConfidence int      `json:"min_confidence"`
			Unreviewed    bool     `json:"unreviewed"`
			Query         string   `json:"query"`
			Regex         bool     `json:"regex"`
			Flow          string   `json:"flow"`
			Limit         int      `json:"limit"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		notes, err := r.notes()
		if err != nil {
			return nil, err
		}
		q := findings.Query{Types: a.Types, Unviewed: a.Unviewed, MinConfidence: a.MinConfidence, UnreviewedOnly: a.Unreviewed}
		if a.Flow != "" {
			_, flowID, err := r.localFlow(a.Flow)
			if err != nil {
				return nil, err
			}
			q.FlowID = flowID
		}
		if a.Query != "" {
			m, err := filter.Compile(a.Query, a.Regex)
			if err != nil {
				return nil, err
			}
			q.Match = m.Match
		}

		matched := notes.Select(q)
		limit := a.Limit
		if limit <= 0 {
			limit = 50
		}
		local := r.router.LocalID()
		out := make([]findingOut, 0, min(limit, len(matched)))
		for _, f := range matched {
			if len(out) >= limit {
				break
			}
			out = append(out, toFindingOut(local, f))
		}
		return map[string]any{
			"findings": out,
			"matched":  len(matched),
			"more":     len(matched) > len(out),
		}, nil
	},
}

var createFinding = &Tool{
	Name: "create_finding",
	Description: "Record a finding: something worth keeping about the traffic — a credential, a leaked " +
		"field, a response that shouldn't have come back. You must explain it in note; a finding " +
		"without one is refused. Pass the flow it was seen in so the user can go straight to it. " +
		"Recording a value that is already a finding adds this sighting to it instead of making a " +
		"second one, so repeat it freely as you find it again elsewhere.",
	Writes: true,
	Schema: object(map[string]any{
		"note":  strField("What this is and why it matters, in your own words. Required."),
		"value": strField("The thing itself: the token, the leaked field, the unexpected body. Findings are deduplicated on it, so pass it exactly as it appeared."),
		"types": map[string]any{
			"type":        "array",
			"items":       map[string]any{"type": "string"},
			"description": "Type ids (or names) classifying the finding; see list_finding_types.",
		},
		"fields": map[string]any{
			"type":                 "object",
			"additionalProperties": map[string]any{"type": "string"},
			"description":          "Extra key/value detail to keep with the finding, e.g. {\"user\": \"admin\"}.",
		},
		"flow":     strField("The flow it was seen in, as a flow handle from list_flows. It must be a flow of this instance."),
		"location": map[string]any{"type": "string", "enum": findings.Locations, "description": "Which part of the message it was in."},
		"detail":   strField("The exact spot, e.g. the header's name."),
		"hidden": map[string]any{
			"type": "boolean",
			"description": "The value is a secret in itself — a password, a key, a session token, a card number, " +
				"a national identifier — so reProxy keeps it masked on screen until the user clicks the eye " +
				"beside it. Set it for anything that would do harm if it were read over a shoulder or caught " +
				"in a screenshot; leave it off for data that is sensitive but not secret, such as an email " +
				"address. It changes the display only: you still read the value back as it is.",
		},
		"confidence": intField("How likely this is a true positive, 0–100: below 50 a probable false positive, above 50 a probable " +
			"true positive, 50 reviewed but genuinely unsure. Omit it to leave the finding unreviewed (confidence null), so the " +
			"findings reviewer scores it. Prefer set_finding_confidence to score findings that already exist."),
		"concern": intField("How big an issue this is from a security or QA standpoint, 0–100: 0 no concern, 100 critical " +
			"(likely to lead to exploitation). Omit it to leave it unassessed. Prefer set_finding_concern to assess findings " +
			"that already exist. A finding both unlikely to be real and not a concern (both below 50) is marked read automatically."),
	}, "note"),
	run: func(ctx context.Context, r *Registry, c Caller, args json.RawMessage) (any, error) {
		var a struct {
			Note       string            `json:"note"`
			Value      string            `json:"value"`
			Types      []string          `json:"types"`
			Fields     map[string]string `json:"fields"`
			Flow       string            `json:"flow"`
			Location   string            `json:"location"`
			Detail     string            `json:"detail"`
			Hidden     bool              `json:"hidden"`
			Confidence *int              `json:"confidence"`
			Concern    *int              `json:"concern"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		notes, err := r.notes()
		if err != nil {
			return nil, err
		}
		if !notes.Recording() {
			return nil, fmt.Errorf("findings are not being recorded in this instance: its engagement " +
				"database could not be opened, so a finding would be lost")
		}
		if err := checkLocation(a.Location); err != nil {
			return nil, err
		}

		draft := findings.Draft{
			Value:      a.Value,
			Types:      a.Types,
			Notes:      strings.TrimSpace(a.Note),
			Fields:     sortedFields(a.Fields),
			Source:     findings.SourceAgent,
			Author:     c.who(),
			Hidden:     a.Hidden,
			Confidence: a.Confidence,
			Concern:    a.Concern,
			Link: findings.Link{
				Location: a.Location,
				Detail:   a.Detail,
			},
		}
		if a.Flow != "" {
			inst, flowID, err := r.localFlow(a.Flow)
			if err != nil {
				return nil, err
			}
			flow, err := inst.GetFlow(ctx, flowID)
			if err != nil {
				return nil, err
			}
			draft.Link.FlowID = flowID
			draft.Link.Method, draft.Link.URL = flow.Method, flow.URL()
		}

		f, created, err := notes.Record(draft)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"finding": toFindingOut(r.router.LocalID(), f),
			"created": created,
			"merged":  !created,
		}, nil
	},
}

var setFindingConfidence = &Tool{
	Name: "set_finding_confidence",
	Description: "Score a finding's confidence from 0 to 100 as you review it, judging from its value, its " +
		"location and the surrounding traffic. 50 means unknown — not enough to tell; below 50 is a probable " +
		"false positive (0 certainly noise); above 50 a probable true positive (100 certainly real). The score " +
		"is kept with the finding: the findings list hides anything below a cutoff (40 by default), so the noise " +
		"you score down drops out of sight while staying on record and searchable. Use it to triage the volume " +
		"as you validate the traffic, so the real findings stand out.",
	Writes: true,
	Schema: object(map[string]any{
		"finding":    strField("The finding to score, as a finding handle from list_findings or create_finding."),
		"confidence": intField("The score, 0–100: 50 unknown, below 50 false positive, above 50 true positive."),
	}, "finding", "confidence"),
	run: func(_ context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Finding    string `json:"finding"`
			Confidence *int   `json:"confidence"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		if a.Confidence == nil {
			return nil, fmt.Errorf("confidence is required (a score from 0 to 100)")
		}
		notes, id, err := r.localFinding(a.Finding)
		if err != nil {
			return nil, err
		}
		if err := notes.SetConfidence(id, a.Confidence); err != nil {
			return nil, err
		}
		f, ok := notes.Get(id)
		if !ok {
			return nil, fmt.Errorf("no finding %s", a.Finding)
		}
		return map[string]any{"finding": toFindingOut(r.router.LocalID(), f)}, nil
	},
}

var setFindingConcern = &Tool{
	Name: "set_finding_concern",
	Description: "Score how big an issue a finding is from a security or QA standpoint, 0 to 100: 0 is no concern, " +
		"100 is critical — likely to lead to exploitation. Judge it from what the finding is and what an attacker " +
		"could do with it. It is kept with the finding beside its confidence; a finding judged both unlikely to be " +
		"real and not a concern (confidence and concern both below 50) is marked read automatically, so the user's " +
		"time goes to what matters.",
	Writes: true,
	Schema: object(map[string]any{
		"finding": strField("The finding to score, as a finding handle from list_findings or create_finding."),
		"concern": intField("The score, 0–100: 0 no concern, 100 critical (likely to lead to exploitation)."),
	}, "finding", "concern"),
	run: func(_ context.Context, r *Registry, _ Caller, args json.RawMessage) (any, error) {
		var a struct {
			Finding string `json:"finding"`
			Concern *int   `json:"concern"`
		}
		if err := unmarshal(args, &a); err != nil {
			return nil, err
		}
		if a.Concern == nil {
			return nil, fmt.Errorf("concern is required (a score from 0 to 100)")
		}
		notes, id, err := r.localFinding(a.Finding)
		if err != nil {
			return nil, err
		}
		if err := notes.SetConcern(id, a.Concern); err != nil {
			return nil, err
		}
		f, ok := notes.Get(id)
		if !ok {
			return nil, fmt.Errorf("no finding %s", a.Finding)
		}
		return map[string]any{"finding": toFindingOut(r.router.LocalID(), f)}, nil
	},
}

// localFinding resolves a finding handle that must name this instance, and
// returns the local findings store to act on it.
func (r *Registry) localFinding(handle string) (FindingStore, uint64, error) {
	instanceID, id, err := parseFinding(handle)
	if err != nil {
		return nil, 0, err
	}
	if instanceID != r.router.LocalID() {
		return nil, 0, fmt.Errorf("findings belong to the engagement of the instance they were recorded in; "+
			"%s is a finding of instance %s, not of %s", handle, instanceID, r.router.LocalID())
	}
	notes, err := r.notes()
	if err != nil {
		return nil, 0, err
	}
	return notes, id, nil
}

// checkLocation rejects a location the UI wouldn't be able to label.
func checkLocation(location string) error {
	if location == "" {
		return nil
	}
	for _, l := range findings.Locations {
		if l == location {
			return nil
		}
	}
	return fmt.Errorf("unknown location %q; the locations are: %s",
		location, strings.Join(findings.Locations, ", "))
}

// sortedFields turns the tool's key/value object into fields in a stable
// order: a JSON object has none, and the user shouldn't see the rows move
// about between calls.
func sortedFields(in map[string]string) []findings.Field {
	if len(in) == 0 {
		return nil
	}
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]findings.Field, 0, len(keys))
	for _, k := range keys {
		out = append(out, findings.Field{Key: k, Value: in[k]})
	}
	return out
}

// localFlow resolves a flow handle that must name this instance. Findings
// live in one process's engagement, so a handle from another instance has
// nowhere to be recorded.
func (r *Registry) localFlow(handle string) (inst instance.Instance, flowID uint64, err error) {
	instanceID, flowID, err := parseFlow(handle)
	if err != nil {
		return nil, 0, err
	}
	if instanceID != r.router.LocalID() {
		return nil, 0, fmt.Errorf("findings are recorded in the engagement of the instance they belong to; "+
			"%s is a flow of instance %s, not of %s", handle, instanceID, r.router.LocalID())
	}
	local, _, err := r.instanceByID(instanceID)
	if err != nil {
		return nil, 0, err
	}
	return local, flowID, nil
}
