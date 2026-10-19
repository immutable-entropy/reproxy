// Package findings is the record of what the work turned up.
//
// A finding is one object of interest: a credential, a leaked field, an
// unexpected response. It carries a value (the thing itself), the types that
// classify it, free-form notes, a key/value store, and a link to every
// request it was seen in. Findings belong to the open engagement and are
// kept in its capture.db (see package archive, which owns that schema), so
// they outlive the session and can be queried by other tools.
//
// There are three ways a finding is raised, and all three go through
// Store.Record with a Draft:
//
//	Source       raised by
//	user         the user, from a selection in a message
//	agent        an AI agent, through the create_finding MCP tool
//	plugin       a plugin acting on an event (once plugins exist)
//
// Record deduplicates on the value: flagging the same value again adds a
// link to the finding that already holds it, unions in any new type and
// appends the note, rather than making a second finding. That is what keeps
// one credential found on twenty endpoints a single finding with twenty
// sightings.
//
// It is Qt-free, and nothing in it runs on the proxy's goroutine: findings
// are written by the user, by agents and by plugins, at their pace, so a
// Store commits synchronously like target.Store rather than queueing.
package findings

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"reproxy/internal/elide"
)

// Type classifies a finding. Types are the user's own taxonomy, kept in the
// settings file and edited in Settings → Findings, so they follow the user
// across engagements; a plugin will be able to declare its own through the
// same catalog (see Store.SetTypes).
type Type struct {
	// ID is the stable identifier a finding stores. It is a slug, e.g.
	// "credential".
	ID string `json:"id"`
	// Name is what the pill reads, e.g. "Credential".
	Name string `json:"name"`
	// Color is the pill's background, as #rrggbb.
	Color string `json:"color"`
}

// Palette is the colours handed to new finding types, in order, picked to
// stay legible as a pill background on every theme. (Package target has its
// own; a target's colour and a finding type's colour are unrelated, so
// neither package reaches into the other.)
var Palette = []string{
	"#d6453d", // red
	"#d08700", // amber
	"#3b6ef5", // blue
	"#8b5cf6", // violet
	"#2e9e4f", // green
	"#0ea5a4", // teal
	"#ec4899", // pink
	"#84a700", // lime
}

// PickColor returns the first palette colour none of used holds, falling
// back to cycling through the palette once every colour is taken.
func PickColor(used []string) string {
	taken := make(map[string]bool, len(used))
	for _, c := range used {
		taken[strings.ToLower(c)] = true
	}
	for _, c := range Palette {
		if !taken[c] {
			return c
		}
	}
	return Palette[len(used)%len(Palette)]
}

// Validate reports whether t can be saved.
func (t Type) Validate() error {
	switch {
	case strings.TrimSpace(t.ID) == "":
		return fmt.Errorf("a finding type needs an id")
	case strings.TrimSpace(t.Name) == "":
		return fmt.Errorf("finding type %q needs a name", t.ID)
	case !isHexColor(t.Color):
		return fmt.Errorf("finding type %q has colour %q, which is not #rrggbb", t.ID, t.Color)
	}
	return nil
}

func isHexColor(s string) bool {
	if len(s) != 7 || s[0] != '#' {
		return false
	}
	for _, r := range s[1:] {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

// Slug turns a type's name into an ID: lower case, with runs of anything but
// letters and digits collapsed to a single dash.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			if dash && b.Len() > 0 {
				b.WriteByte('-')
			}
			dash = false
			b.WriteRune(r)
		default:
			dash = true
		}
	}
	return b.String()
}

// Source says who raised a finding.
type Source string

const (
	// SourceUser is a finding the user made from a selection in a message.
	// It needs no notification: they just made it.
	SourceUser Source = "user"
	// SourceAgent is a finding an AI agent raised. It always carries a note
	// (the tool refuses a blank one) and arrives unviewed.
	SourceAgent Source = "agent"
	// SourcePlugin is a finding a plugin raised. It arrives unviewed.
	SourcePlugin Source = "plugin"
)

// Raised reports whether a finding from this source should be announced to
// the user: anything they didn't make themselves.
func (s Source) Raised() bool { return s != SourceUser }

// Confidence runs from 0 to 100, or is absent (nil) until a finding is
// reviewed. The midpoint is the "reviewed, but genuinely unsure" score;
// below it are shades of false positive, above it shades of true positive.
// The floor is the default cutoff below which a reviewed finding is treated
// as noise — hidden from the list by default (the findings page sets its
// filter to it) and left out of the unviewed badge. An unreviewed finding
// has no score and is never hidden by the cutoff.
const (
	ConfidenceMin     = 0
	ConfidenceMax     = 100
	ConfidenceUnknown = 50
	ConfidenceFloor   = 40
)

// Concern runs from 0 to 100: 0 is no concern, 100 is critical — likely to
// lead to exploitation — or absent (nil) until a finding is assessed. It
// shares the 0–100 range with Confidence.
const (
	ConcernMin = 0
	ConcernMax = 100
)

// autoReadBelow is the score under which a finding is low-value on an axis.
// When both its confidence and its concern are assessed and below it, the
// finding is marked read automatically (see maybeAutoRead): unlikely to be
// real and not a concern together means it isn't worth the user's time.
const autoReadBelow = 50

// clampScore holds a confidence or concern value to the 0–100 range, so a bad
// value from a tool or a hand edit can't put a finding out of range.
func clampScore(c int) int {
	switch {
	case c < ConfidenceMin:
		return ConfidenceMin
	case c > ConfidenceMax:
		return ConfidenceMax
	default:
		return c
	}
}

// Where in a message a finding was seen. The empty value means the finding
// isn't tied to a particular part.
const (
	InRequestHeader  = "request_header"
	InRequestBody    = "request_body"
	InResponseHeader = "response_header"
	InResponseBody   = "response_body"
	// InRecoveredFile pins a sighting to a file the work recovered (package
	// files) rather than to a message: an app's own source a source map
	// carried, say. Such a Link has no flow (FlowID 0); its Detail is the
	// file's path under the engagement's recovered-files root, which is what
	// resolves the file (files.ByPath) when the user opens the link.
	InRecoveredFile = "recovered_file"
)

// Locations lists the message parts a finding can be pinned to, for a tool
// schema. InRecoveredFile is deliberately not here: a file link is made by
// naming the file, not by the agent choosing a location.
var Locations = []string{InRequestHeader, InRequestBody, InResponseHeader, InResponseBody}

// Field is one entry of a finding's key/value store. They are a slice, not a
// map: the order is the user's, and the UI shows the rows as they typed them.
type Field struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// Link is one sighting: the request a finding was seen in, and where in it.
//
// Method, URL, Location and Detail are a snapshot taken when the link was
// made, so a finding still reads "GET https://host/login — response header"
// after the traffic it came from has been cleared. FlowID stops resolving
// then; the finding itself is unaffected.
type Link struct {
	// FlowID is the capture flow, or 0 when no request is known.
	FlowID uint64 `json:"flow_id,omitempty"`
	Method string `json:"method,omitempty"`
	URL    string `json:"url,omitempty"`
	// Location is one of the In* constants, or "" for a finding that isn't
	// pinned to one part of the message.
	Location string `json:"location,omitempty"`
	// Detail names the exact spot, e.g. the header's name.
	Detail string    `json:"detail,omitempty"`
	Added  time.Time `json:"added"`
}

// same reports whether two links point at the same spot, so a value flagged
// twice in one place doesn't collect duplicate sightings.
func (l Link) same(o Link) bool {
	return l.FlowID == o.FlowID && l.Location == o.Location && l.Detail == o.Detail
}

// empty reports whether a link says nothing worth recording.
func (l Link) empty() bool {
	return l.FlowID == 0 && l.Method == "" && l.URL == "" && l.Location == "" && l.Detail == ""
}

// Label describes where this sighting was, for the UI and tool output.
func (l Link) Label() string {
	s := strings.TrimSpace(l.Method + " " + l.URL)
	if where := LocationLabel(l.Location); where != "" {
		if s == "" {
			s = where
		} else {
			s += " — " + where
		}
	}
	if l.Detail != "" {
		s += " (" + l.Detail + ")"
	}
	return s
}

// LocationLabel is how a location reads in the UI.
func LocationLabel(location string) string {
	switch location {
	case InRequestHeader:
		return "request header"
	case InRequestBody:
		return "request body"
	case InResponseHeader:
		return "response header"
	case InResponseBody:
		return "response body"
	case InRecoveredFile:
		return "recovered file"
	}
	return ""
}

// RecoveredFileLink makes a sighting that points at a recovered file by its
// path under the recovered-files root. The path goes in Detail (not URL) so
// that two file links on one finding stay distinct rows in finding_flows,
// whose key includes the detail.
func RecoveredFileLink(path string) Link {
	return Link{Location: InRecoveredFile, Detail: path}
}

// Finding is one object of interest.
type Finding struct {
	// ID is the finding's row in the engagement's database, so it is stable
	// across sessions.
	ID uint64 `json:"id"`
	// Value is the thing itself: the token, the leaked field, the body that
	// shouldn't have come back. It may be empty for a finding that is a
	// note about a request rather than about a value in it.
	Value string `json:"value,omitempty"`
	// Types are the type IDs classifying this finding. A finding with none
	// shows as untyped rather than being refused.
	Types []string `json:"types,omitempty"`
	// Notes is free-form text. Findings raised by an agent or a plugin
	// always have some.
	Notes string `json:"notes,omitempty"`
	// Fields is the finding's key/value store, empty by default.
	Fields []Field `json:"fields,omitempty"`
	// Source is who raised it; Author names the agent session or plugin.
	Source Source `json:"source"`
	Author string `json:"author,omitempty"`
	// Viewed is false until the user opens the finding. The Findings badge
	// counts the findings that are not.
	Viewed bool `json:"viewed"`
	// Hidden says the value is a secret in itself — a password, a key, a
	// session token, a card number, a national identifier — so the UI keeps
	// it masked until the user asks to see it (the eye beside the value).
	// It conceals the value on screen and nothing else: the finding is
	// stored, searched and handed to agents and plugins as it always was,
	// because a value an agent can't read is a value it can't test.
	Hidden bool `json:"hidden,omitempty"`
	// Confidence is a judgement of how likely this finding is a true
	// positive, from 0 to 100 (see the Confidence constants), or nil when it
	// has not been reviewed yet. A nil score is deliberately distinct from a
	// deliberate 50 ("reviewed, but genuinely unsure"): without that
	// distinction the reviewer would keep re-reviewing the same findings. A
	// new finding starts unreviewed (nil) until an agent or the user scores
	// it. Below ConfidenceFloor is treated as noise — filtered out of the
	// list by default and left out of the unviewed badge — while unreviewed
	// and higher-scored findings stay in view. It is stored and shown to
	// agents either way, because a value an agent can't read is one it can't
	// test.
	Confidence *int `json:"confidence"`
	// Concern is how big an issue this finding is from a security or QA
	// standpoint, from 0 to 100 — 0 is no concern, 100 is critical (likely to
	// lead to exploitation) — or nil when it has not been assessed yet. Like
	// Confidence, a nil value is distinct from a deliberate 0 ("assessed, no
	// concern"). A finding judged both unlikely to be real and not a concern
	// (both scores below the auto-read line) is marked read automatically, so
	// low-value findings don't take the user's time (see maybeAutoRead).
	Concern *int `json:"concern"`
	// Links are the sightings, oldest first.
	Links   []Link    `json:"links,omitempty"`
	Created time.Time `json:"created"`
	Updated time.Time `json:"updated"`
}

// HasType reports whether the finding carries a type.
func (f Finding) HasType(id string) bool { return slices.Contains(f.Types, id) }

// Summary is the one-line description of a finding, for a list row that has
// no room for the value itself or for tool output.
func (f Finding) Summary() string {
	if v := collapse(f.Value); v != "" {
		return v
	}
	if n := collapse(f.Notes); n != "" {
		return n
	}
	return "(no value)"
}

// Masked is how a hidden value reads while it is concealed: nothing of the
// value itself, and its length, which is often all a tester needs to tell
// two of them apart.
func (f Finding) Masked() string {
	n := len([]rune(f.Value))
	if n == 0 {
		return ""
	}
	dots := n
	if dots > 8 {
		dots = 8
	}
	return fmt.Sprintf("%s (%d characters)", strings.Repeat("•", dots), n)
}

// SummaryMasked is Summary for a view on screen: for a hidden finding it
// says what the finding is (its notes) rather than what the value is, so a
// list of concealed credentials is still a list you can read.
func (f Finding) SummaryMasked() string {
	if !f.Hidden || f.Value == "" {
		return f.Summary()
	}
	if n := collapse(f.Notes); n != "" {
		return "••• " + n
	}
	return f.Masked()
}

// SearchText is what a text filter matches a finding against: its value, its
// notes, its fields and the requests it was seen in. Keeping it here (not in
// the UI) means the Findings filter, tool output and later plugins share one
// notion of a finding's searchable content.
func (f Finding) SearchText() string {
	var b strings.Builder
	b.WriteString(f.Value)
	b.WriteByte('\n')
	b.WriteString(f.Notes)
	for _, kv := range f.Fields {
		b.WriteByte('\n')
		b.WriteString(kv.Key)
		b.WriteByte(' ')
		b.WriteString(kv.Value)
	}
	for _, l := range f.Links {
		b.WriteByte('\n')
		b.WriteString(l.Label())
	}
	return b.String()
}

// clone returns a deep copy, so a Finding handed out by a Store can never
// alias the slices the Store still owns.
func (f Finding) clone() Finding {
	f.Types = append([]string(nil), f.Types...)
	f.Fields = append([]Field(nil), f.Fields...)
	f.Links = append([]Link(nil), f.Links...)
	if f.Confidence != nil {
		c := *f.Confidence
		f.Confidence = &c
	}
	if f.Concern != nil {
		c := *f.Concern
		f.Concern = &c
	}
	return f
}

// collapse trims a string and folds its whitespace onto one line, cutting it
// to something a row can show.
func collapse(s string) string {
	return elide.Runes(strings.Join(strings.Fields(s), " "), 160)
}

// Draft is a finding being recorded. It is the one input to Store.Record,
// whichever of the three routes is asking.
type Draft struct {
	// Value is deduplicated on: a draft whose value a finding already holds
	// is merged into it. An empty value never merges.
	Value string
	// Types are type IDs or names; Record resolves them against the catalog
	// and refuses one it doesn't know.
	Types []string
	Notes string
	// Fields are merged key by key; a key the finding already has is left
	// as it stands rather than overwritten.
	Fields []Field
	// Source and Author say who is recording this. An empty Source is
	// treated as SourceUser.
	Source Source
	Author string
	// Link is the sighting, if one is known.
	Link Link
	// Links are further sightings recorded in the same breath — a finding
	// that points at several recovered files, say. They are added alongside
	// Link, each deduplicated against what the finding already holds.
	Links []Link
	// Hidden asks for the value to be masked on screen (see
	// Finding.Hidden). Merging a hidden draft into a finding hides it: one
	// sighting of a value as a secret is enough.
	Hidden bool
	// Confidence scores the finding 0–100 as it is recorded (see
	// Finding.Confidence), for an agent that judges a value as it raises it.
	// Nil leaves a new finding unreviewed and leaves an existing one's score
	// alone on a merge; a value is clamped to range. Afterwards only
	// SetConfidence changes it.
	Confidence *int
	// Concern scores how big an issue the finding is 0–100 as it is recorded
	// (see Finding.Concern), the same way as Confidence: nil leaves it
	// unassessed and leaves an existing one alone on a merge; afterwards only
	// SetConcern changes it.
	Concern *int
}

// Key is the dedup key: the value with surrounding whitespace removed and
// nothing else changed, since a secret differing only in case is a different
// secret. An empty key never matches another finding.
func (d Draft) Key() string { return strings.TrimSpace(d.Value) }

// normalized tidies a draft before it is recorded or matched.
func (d Draft) normalized() Draft {
	d.Notes = strings.TrimRight(d.Notes, " \t\r\n")
	if d.Source == "" {
		d.Source = SourceUser
	}
	d.Types = dedupeStrings(d.Types)
	d.Fields = trimFields(d.Fields)
	return d
}

// validate reports whether the draft says anything worth recording.
func (d Draft) validate() error {
	switch d.Source {
	case SourceUser, SourceAgent, SourcePlugin:
	default:
		return fmt.Errorf("unknown finding source %q", d.Source)
	}
	if strings.TrimSpace(d.Value) == "" && strings.TrimSpace(d.Notes) == "" {
		return fmt.Errorf("a finding needs a value or a note")
	}
	if d.Source.Raised() && strings.TrimSpace(d.Notes) == "" {
		return fmt.Errorf("a finding raised by an %s must explain itself in a note", d.Source)
	}
	return nil
}

func dedupeStrings(in []string) []string {
	if len(in) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func trimFields(in []Field) []Field {
	out := make([]Field, 0, len(in))
	for _, kv := range in {
		kv.Key = strings.TrimSpace(kv.Key)
		if kv.Key == "" {
			continue // a value with no key can't be looked up; drop it
		}
		out = append(out, kv)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// fieldsJSON and parseFields keep a finding's key/value store in one column,
// as an ordered JSON array. It is the user's own free-form data: there is
// nothing to query it by, and the order matters, so a table of rows would
// cost more than it gives.
func fieldsJSON(fields []Field) (string, error) {
	if len(fields) == 0 {
		return "[]", nil
	}
	data, err := json.Marshal(fields)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func parseFields(s string) []Field {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	var out []Field
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		// A hand-edited or future-format column costs the key/value store,
		// not the finding.
		return nil
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
