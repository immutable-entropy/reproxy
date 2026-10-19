// Package manifest is a plugin's plugin.json: what the plugin is, which of
// its functions run on which hooks (and for which traffic), and the settings
// it offers. It parses and validates the file — every problem, not just the
// first, so the Settings page can list them — and compiles each target's
// match filter into a Matcher that core evaluates in Go, so traffic no
// plugin wants never reaches the TypeScript runtime.
//
// It depends on nothing internal.
package manifest

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"regexp"
	"strconv"
	"strings"
)

// FileName is the manifest in a plugin's directory; EntryName is the script
// it starts from. Everything else a plugin needs is imported from there.
const (
	FileName  = "plugin.json"
	EntryName = "main.ts"
)

// SDKVersion is the major version of the plugin SDK this build provides. A
// manifest written for another is refused rather than half-working.
const SDKVersion = "1"

// Status is whether a plugin runs.
type Status string

const (
	Enabled  Status = "enabled"
	Disabled Status = "disabled"
)

// Manifest is a parsed plugin.json.
type Manifest struct {
	Name        string `json:"name"`
	Version     string `json:"version"`
	Description string `json:"description,omitempty"`
	Author      Author `json:"author"`
	// Status is the author's default; the user's choice in Settings wins.
	Status Status `json:"status"`
	// SDK is the SDK major version the plugin was written against.
	SDK string `json:"sdk,omitempty"`
	// FindingTypes are types the plugin records findings as. They are added
	// to the catalog while the plugin is enabled, so its findings classify
	// even when the user has no such type.
	FindingTypes []FindingType `json:"finding_types,omitempty"`
	Targets      []Target      `json:"targets,omitempty"`
	// Actions are what the plugin offers to run on a flow the user picks
	// (the traffic list's context menu).
	Actions []Action `json:"actions,omitempty"`
	// Commands are what the plugin adds to reproxy's command line, under
	// the plugin's own name (see command.go).
	Commands []Command `json:"commands,omitempty"`
	Settings []Setting `json:"settings,omitempty"`

	// version is Version parsed.
	version Version
}

// Author is who wrote the plugin.
type Author struct {
	Name  string `json:"name"`
	Email string `json:"email,omitempty"`
}

// FindingType is a finding type a plugin declares.
type FindingType struct {
	ID    string `json:"id,omitempty"` // derived from the name when empty
	Name  string `json:"name"`
	Color string `json:"color,omitempty"`
}

// Target maps a hook to one of the plugin's exported functions.
type Target struct {
	Hook     Hook   `json:"hook"`
	Function string `json:"function"`
	// Match narrows which events reach the function. It is evaluated in Go.
	Match *Match `json:"match,omitempty"`
	// Self lets the function see the plugin's own work — traffic it sent,
	// or files it recovered — which it is not told about otherwise. Off by
	// default, which is what keeps a plugin that sends requests from
	// triggering itself; never allowed on intercept hooks.
	Self bool `json:"self,omitempty"`
	// Hold, on an intercept hook, has core hold the messages the match
	// filter accepts for the plugin, whether or not the user's interception
	// is on. A message held only this way never reaches the user: it goes
	// on once the plugins have answered.
	Hold bool `json:"hold,omitempty"`
	// Priority orders plugins on an intercept hook, lowest first.
	Priority int `json:"priority,omitempty"`
	// TimeoutMS bounds an intercept hook's answer.
	TimeoutMS int `json:"timeout_ms,omitempty"`
}

// Action is something the user can run on one flow, from the traffic list's
// context menu. Its function is called with the flow; nothing calls it
// otherwise.
type Action struct {
	// ID names the action within the plugin: lower-case letters, digits and
	// dashes.
	ID string `json:"id"`
	// Label is the menu item.
	Label string `json:"label"`
	// Description is the menu item's tooltip.
	Description string `json:"description,omitempty"`
	Function    string `json:"function"`
	// Match narrows the flows the action is offered for, as a traffic
	// hook's does (content_types tests the response). Unlike a traffic
	// hook's, it accepts flows from every source by default, the plugins'
	// own included: the user chose the flow.
	Match *Match `json:"match,omitempty"`
}

// MaxLabel is the longest an action's label may be.
const MaxLabel = 80

// Timeout limits for decide hooks.
const (
	DefaultTimeoutMS = 2000
	MinTimeoutMS     = 100
	MaxTimeoutMS     = 30000
)

// Setting is one setting a plugin offers the user.
type Setting struct {
	Key         string `json:"key"`
	Type        string `json:"type,omitempty"` // inferred from Default when empty
	Label       string `json:"label,omitempty"`
	Description string `json:"description,omitempty"`
	// Default is the value until the user changes it.
	Default json.RawMessage `json:"default,omitempty"`
	// Options are an enum's choices.
	Options []string `json:"options,omitempty"`
}

// Setting types.
const (
	TypeString  = "string"
	TypeNumber  = "number"
	TypeBoolean = "boolean"
	TypeSecret  = "secret" // a string kept in the secrets file, never in config
	TypeEnum    = "enum"
	TypeList    = "list" // a list of strings
)

// Errors is every problem found in a manifest.
type Errors []error

func (e Errors) Error() string {
	msgs := make([]string, len(e))
	for i, err := range e {
		msgs[i] = err.Error()
	}
	return strings.Join(msgs, "; ")
}

var (
	nameRe  = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{1,63}$`)
	identRe = regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	keyRe   = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	// actionIDRe is an action's id.
	actionIDRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,63}$`)
)

// Parse reads and validates a plugin.json. dir is the name of the directory
// it came from, which the plugin's name must equal. A manifest that parses
// but has problems is returned along with them (as Errors), so what can be
// shown about the plugin still is.
func Parse(data []byte, dir string) (*Manifest, error) {
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return nil, fmt.Errorf("%s: %w", FileName, err)
	}
	if errs := m.validate(dir); len(errs) > 0 {
		return &m, errs
	}
	return &m, nil
}

func (m *Manifest) validate(dir string) Errors {
	var errs Errors
	add := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	switch {
	case !nameRe.MatchString(m.Name):
		add("name %q must be 2-64 lower-case letters, digits and dashes", m.Name)
	case dir != "" && m.Name != dir:
		add("name %q must match the plugin's directory, %q", m.Name, dir)
	}
	v, err := ParseVersion(m.Version)
	if err != nil {
		add("%v", err)
	}
	m.version = v
	if strings.TrimSpace(m.Author.Name) == "" {
		add("author.name is required")
	}
	if m.Author.Email != "" {
		if a, err := mail.ParseAddress(m.Author.Email); err != nil || a.Address != m.Author.Email {
			add("author.email %q is not an email address", m.Author.Email)
		}
	}
	switch m.Status {
	case Enabled, Disabled:
	case "":
		add("status is required: enabled or disabled")
	default:
		add("status %q must be enabled or disabled", m.Status)
	}
	if m.SDK != "" && m.SDK != SDKVersion {
		add("written for SDK %s; this build provides SDK %s", m.SDK, SDKVersion)
	}
	for i, t := range m.FindingTypes {
		if strings.TrimSpace(t.Name) == "" {
			add("finding_types[%d] needs a name", i)
		}
	}

	if len(m.Targets) == 0 && len(m.Actions) == 0 && len(m.Commands) == 0 {
		add("targets must map at least one hook to a function, or actions or commands must offer one")
	}
	for i := range m.Targets {
		for _, err := range m.Targets[i].validate() {
			add("targets[%d] (%s): %v", i, m.Targets[i].Hook, err)
		}
	}

	ids := map[string]bool{}
	for i := range m.Actions {
		a := &m.Actions[i]
		if ids[a.ID] {
			add("actions[%d]: id %q is used twice", i, a.ID)
		}
		ids[a.ID] = true
		for _, err := range a.validate() {
			add("actions[%d] (%s): %v", i, a.ID, err)
		}
	}

	cmds := map[string]bool{}
	for i := range m.Commands {
		c := &m.Commands[i]
		if cmds[c.Action] {
			add("commands[%d]: the action %q is declared twice", i, c.Action)
		}
		cmds[c.Action] = true
		for _, err := range c.validate() {
			add("commands[%d] (%s): %v", i, c.Action, err)
		}
	}

	seen := map[string]bool{}
	for i := range m.Settings {
		s := &m.Settings[i]
		if seen[s.Key] {
			add("settings[%d]: key %q is used twice", i, s.Key)
		}
		seen[s.Key] = true
		if err := s.validate(); err != nil {
			add("settings[%d] (%s): %v", i, s.Key, err)
		}
	}
	return errs
}

func (t *Target) validate() []error {
	var errs []error
	info, ok := Lookup(t.Hook)
	if !ok {
		errs = append(errs, fmt.Errorf("no hook %q (see the hook catalog)", t.Hook))
	}
	if !identRe.MatchString(t.Function) {
		errs = append(errs, fmt.Errorf("function %q is not a function name", t.Function))
	}
	if !ok {
		return errs
	}
	if t.Match != nil {
		if info.Subject == SubjectNone {
			errs = append(errs, errors.New("this hook takes no match filter"))
		} else if _, err := t.Match.Compile(info); err != nil {
			errs = append(errs, err)
		}
	}
	if info.Class == Decide {
		if t.Self {
			errs = append(errs, errors.New("self is not allowed on intercept hooks"))
		}
		if t.TimeoutMS != 0 && (t.TimeoutMS < MinTimeoutMS || t.TimeoutMS > MaxTimeoutMS) {
			errs = append(errs, fmt.Errorf("timeout_ms must be between %d and %d", MinTimeoutMS, MaxTimeoutMS))
		}
	} else {
		if t.Hold {
			errs = append(errs, errors.New("hold only applies to intercept hooks"))
		}
		if t.Priority != 0 {
			errs = append(errs, errors.New("priority only applies to intercept hooks"))
		}
		if t.TimeoutMS != 0 {
			errs = append(errs, errors.New("timeout_ms only applies to intercept hooks"))
		}
	}
	if t.Self && info.Subject != SubjectFlow && info.Subject != SubjectFile {
		errs = append(errs, errors.New("self only applies to traffic and file hooks"))
	}
	return errs
}

func (a *Action) validate() []error {
	var errs []error
	if !actionIDRe.MatchString(a.ID) {
		errs = append(errs, errors.New("id must be 1-64 lower-case letters, digits and dashes"))
	}
	label := strings.TrimSpace(a.Label)
	switch {
	case label == "":
		errs = append(errs, errors.New("label is required: it is the menu item"))
	case len([]rune(label)) > MaxLabel || strings.ContainsAny(label, "\r\n"):
		errs = append(errs, fmt.Errorf("label must be one line of at most %d characters", MaxLabel))
	}
	if !identRe.MatchString(a.Function) {
		errs = append(errs, fmt.Errorf("function %q is not a function name", a.Function))
	}
	if _, err := a.Match.CompileAction(); err != nil {
		errs = append(errs, err)
	}
	return errs
}

// Timeout is the target's decide timeout in milliseconds.
func (t *Target) Timeout() int {
	if t.TimeoutMS == 0 {
		return DefaultTimeoutMS
	}
	return t.TimeoutMS
}

func (s *Setting) validate() error {
	if !keyRe.MatchString(s.Key) {
		return errors.New("key must be lower-case letters, digits and underscores, starting with a letter")
	}
	if s.Type == "" {
		s.Type = inferType(s.Default)
		if s.Type == "" {
			return errors.New("needs a type, or a default to infer it from")
		}
	}
	switch s.Type {
	case TypeString, TypeNumber, TypeBoolean, TypeSecret, TypeList:
		if len(s.Options) > 0 {
			return errors.New("options only apply to enum settings")
		}
	case TypeEnum:
		if len(s.Options) == 0 {
			return errors.New("an enum needs options")
		}
	default:
		return fmt.Errorf("type %q must be string, number, boolean, secret, enum or list", s.Type)
	}
	if s.Type == TypeSecret && len(s.Default) > 0 && string(s.Default) != `""` && string(s.Default) != "null" {
		return errors.New("a secret can't have a default: it would be in the plugin's files")
	}
	if len(s.Default) > 0 {
		if err := s.Check(s.Default); err != nil {
			return fmt.Errorf("default: %w", err)
		}
	}
	return nil
}

// Check reports whether v is a valid value for the setting.
func (s *Setting) Check(v json.RawMessage) error {
	if string(v) == "null" {
		return nil
	}
	var x any
	if err := json.Unmarshal(v, &x); err != nil {
		return err
	}
	switch s.Type {
	case TypeString, TypeSecret:
		if _, ok := x.(string); !ok {
			return errors.New("must be a string")
		}
	case TypeNumber:
		if _, ok := x.(float64); !ok {
			return errors.New("must be a number")
		}
	case TypeBoolean:
		if _, ok := x.(bool); !ok {
			return errors.New("must be true or false")
		}
	case TypeEnum:
		str, ok := x.(string)
		if !ok {
			return errors.New("must be one of the options")
		}
		for _, o := range s.Options {
			if o == str {
				return nil
			}
		}
		return fmt.Errorf("%q is not one of %s", str, strings.Join(s.Options, ", "))
	case TypeList:
		list, ok := x.([]any)
		if !ok {
			return errors.New("must be a list of strings")
		}
		for _, item := range list {
			if _, ok := item.(string); !ok {
				return errors.New("must be a list of strings")
			}
		}
	}
	return nil
}

// Zero is the value a setting has with neither a default nor a choice.
func (s *Setting) Zero() json.RawMessage {
	switch s.Type {
	case TypeNumber:
		return json.RawMessage("0")
	case TypeBoolean:
		return json.RawMessage("false")
	case TypeList:
		return json.RawMessage("[]")
	case TypeEnum:
		b, _ := json.Marshal(s.Options[0])
		return b
	}
	return json.RawMessage(`""`)
}

func inferType(v json.RawMessage) string {
	var x any
	if len(v) == 0 || json.Unmarshal(v, &x) != nil {
		return ""
	}
	switch x.(type) {
	case string:
		return TypeString
	case float64:
		return TypeNumber
	case bool:
		return TypeBoolean
	case []any:
		return TypeList
	}
	return ""
}

// Setting returns the setting with the given key.
func (m *Manifest) Setting(key string) (*Setting, bool) {
	for i := range m.Settings {
		if m.Settings[i].Key == key {
			return &m.Settings[i], true
		}
	}
	return nil, false
}

// ParsedVersion returns Version, parsed.
func (m *Manifest) ParsedVersion() Version { return m.version }

// ---- Versions -------------------------------------------------------------

// Version is a plugin version. Two forms are accepted:
//
//   - major.minor[-sub], where sub is a build counter: 1.0-2 is newer than
//     1.0-1, which is newer than 1.0;
//   - semver, major.minor.patch[-pre], where a pre-release comes before the
//     release: 1.2.3-beta.1 is older than 1.2.3.
type Version struct {
	Major, Minor, Patch int
	// HasPatch is set for the semver form.
	HasPatch bool
	// Suffix is the part after the dash: a build counter, or a pre-release.
	Suffix string
}

var versionRe = regexp.MustCompile(`^(\d+)\.(\d+)(?:\.(\d+))?(?:-([0-9A-Za-z]+(?:\.[0-9A-Za-z]+)*))?$`)

// ParseVersion parses a version string.
func ParseVersion(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("version %q must be major.minor, major.minor-sub or major.minor.patch", s)
	}
	var v Version
	v.Major, _ = strconv.Atoi(m[1])
	v.Minor, _ = strconv.Atoi(m[2])
	if m[3] != "" {
		v.Patch, _ = strconv.Atoi(m[3])
		v.HasPatch = true
	}
	v.Suffix = m[4]
	if !v.HasPatch && v.Suffix != "" {
		if _, err := strconv.Atoi(v.Suffix); err != nil {
			return Version{}, fmt.Errorf("version %q: the sub in major.minor-sub must be a number", s)
		}
	}
	return v, nil
}

func (v Version) String() string {
	s := fmt.Sprintf("%d.%d", v.Major, v.Minor)
	if v.HasPatch {
		s += fmt.Sprintf(".%d", v.Patch)
	}
	if v.Suffix != "" {
		s += "-" + v.Suffix
	}
	return s
}

// Compare returns -1, 0 or 1 as v is older than, the same as, or newer than
// w.
func (v Version) Compare(w Version) int {
	for _, d := range [][2]int{{v.Major, w.Major}, {v.Minor, w.Minor}, {v.Patch, w.Patch}} {
		if d[0] != d[1] {
			return cmp(d[0], d[1])
		}
	}
	vb, wb := v.build(), w.build()
	if vb != wb {
		return cmp(vb, wb)
	}
	// Semver pre-releases: none is newer than any.
	vp, wp := v.pre(), w.pre()
	switch {
	case vp == wp:
		return 0
	case vp == "":
		return 1
	case wp == "":
		return -1
	}
	return comparePre(vp, wp)
}

// build is the sub of the major.minor-sub form, 0 otherwise.
func (v Version) build() int {
	if v.HasPatch || v.Suffix == "" {
		return 0
	}
	n, _ := strconv.Atoi(v.Suffix)
	return n
}

// pre is the semver pre-release.
func (v Version) pre() string {
	if !v.HasPatch {
		return ""
	}
	return v.Suffix
}

func comparePre(a, b string) int {
	as, bs := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(as) && i < len(bs); i++ {
		an, aerr := strconv.Atoi(as[i])
		bn, berr := strconv.Atoi(bs[i])
		switch {
		case aerr == nil && berr == nil:
			if an != bn {
				return cmp(an, bn)
			}
		case aerr == nil:
			return -1 // numeric identifiers sort first
		case berr == nil:
			return 1
		default:
			if c := strings.Compare(as[i], bs[i]); c != 0 {
				return c
			}
		}
	}
	return cmp(len(as), len(bs))
}

func cmp(a, b int) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}
