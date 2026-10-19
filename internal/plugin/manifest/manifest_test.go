package manifest

import (
	"encoding/json"
	"strings"
	"testing"
)

const good = `{
	"name": "sensitive-info", "version": "1.0-1", "description": "d",
	"author": {"name": "Ann", "email": "ann@example.test"}, "status": "disabled", "sdk": "1",
	"targets": [
		{"hook": "traffic.on_response_received", "function": "scan",
		 "match": {"scope": "in_scope", "hosts": ["*.app.test"], "status": ["2xx"], "content_types": ["application/*+json", "text/*"]}},
		{"hook": "intercept.on_request_pending", "function": "tag", "priority": 5, "timeout_ms": 500},
		{"hook": "plugin.on_load", "function": "init"}
	],
	"settings": [
		{"key": "min_entropy", "label": "Minimum entropy", "default": 3.5},
		{"key": "mode", "type": "enum", "options": ["fast", "thorough"], "default": "fast"},
		{"key": "token", "type": "secret"}
	]
}`

func TestParseGood(t *testing.T) {
	m, err := Parse([]byte(good), "sensitive-info")
	if err != nil {
		t.Fatal(err)
	}
	if s, _ := m.Setting("min_entropy"); s.Type != TypeNumber {
		t.Errorf("inferred type = %q", s.Type)
	}
	if m.Targets[1].Timeout() != 500 || m.Targets[2].Timeout() != DefaultTimeoutMS {
		t.Error("timeouts")
	}
}

func TestParseCollectsEveryProblem(t *testing.T) {
	bad := `{
		"name": "Bad Name", "version": "one", "author": {"email": "nope"}, "status": "on", "sdk": "9",
		"targets": [
			{"hook": "traffic.on_nothing", "function": "f"},
			{"hook": "traffic.on_flow_complete", "function": "not a name", "priority": 1},
			{"hook": "plugin.on_load", "function": "f", "match": {"hosts": ["x"]}},
			{"hook": "intercept.on_request_pending", "function": "f", "self": true, "timeout_ms": 5},
			{"hook": "traffic.on_flow_complete", "function": "f", "match": {"status": ["9xx"], "path_regex": "(", "sources": ["browser"]}}
		],
		"settings": [{"key": "a", "default": 1}, {"key": "a", "type": "enum"}, {"key": "s", "type": "secret", "default": "hunter2"}]
	}`
	_, err := Parse([]byte(bad), "bad")
	errs, ok := err.(Errors)
	if !ok {
		t.Fatalf("err = %v", err)
	}
	text := err.Error()
	for _, want := range []string{
		"name \"Bad Name\"", "version \"one\"", "author.name is required", "author.email",
		"status \"on\"", "SDK 9", "no hook \"traffic.on_nothing\"", "function \"not a name\"",
		"priority only applies", "takes no match", "self is not allowed", "timeout_ms must be",
		"9xx", "path_regex", "\"browser\"", "key \"a\" is used twice", "an enum needs options",
		"a secret can't have a default",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in:\n%s", want, strings.ReplaceAll(text, "; ", "\n"))
		}
	}
	if len(errs) < 15 {
		t.Errorf("only %d errors", len(errs))
	}
}

func TestNameMustMatchDirectory(t *testing.T) {
	if _, err := Parse([]byte(good), "other"); err == nil || !strings.Contains(err.Error(), "must match the plugin's directory") {
		t.Fatalf("err = %v", err)
	}
}

func TestUnknownFieldsAreRefused(t *testing.T) {
	if _, err := Parse([]byte(`{"name":"x1","colour":"red"}`), ""); err == nil {
		t.Fatal("an unknown field was accepted")
	}
}

func TestVersions(t *testing.T) {
	order := []string{"0.9", "1.0", "1.0-1", "1.0-2", "1.0-10", "1.1", "1.1.1-alpha", "1.1.1-alpha.2", "1.1.1-beta", "1.1.1", "2.0.0"}
	for i := 1; i < len(order); i++ {
		a, err1 := ParseVersion(order[i-1])
		b, err2 := ParseVersion(order[i])
		if err1 != nil || err2 != nil {
			t.Fatal(err1, err2)
		}
		if a.Compare(b) != -1 || b.Compare(a) != 1 {
			t.Errorf("%s should be older than %s", order[i-1], order[i])
		}
	}
	for _, bad := range []string{"1", "1.0-x", "v1.0", "1.0.0.0", "1.0-"} {
		if _, err := ParseVersion(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	if v, _ := ParseVersion("1.2.3-rc.1"); v.String() != "1.2.3-rc.1" {
		t.Errorf("String = %q", v.String())
	}
}

func TestSettingCheck(t *testing.T) {
	enum := Setting{Key: "m", Type: TypeEnum, Options: []string{"a", "b"}}
	list := Setting{Key: "l", Type: TypeList}
	for _, c := range []struct {
		s   Setting
		v   string
		bad bool
	}{
		{enum, `"a"`, false}, {enum, `"c"`, true}, {list, `["x","y"]`, false}, {list, `["x",1]`, true},
		{Setting{Type: TypeNumber}, `"1"`, true}, {Setting{Type: TypeBoolean}, `true`, false},
	} {
		if err := c.s.Check(json.RawMessage(c.v)); (err != nil) != c.bad {
			t.Errorf("Check(%s %s) = %v", c.s.Type, c.v, err)
		}
	}
}

func flowFacts() *Facts {
	return &Facts{
		Source: "proxy", TargetID: "t1", Method: "POST", Status: 201, Host: "API.App.test:443",
		Path: "/api/v1/users?x=1",
		Head: []byte("HTTP/1.1 201 Created\r\nServer: x\r\ncontent-TYPE: Application/Problem+JSON; charset=utf-8\r\n\r\n{}"),
	}
}

func compile(t *testing.T, hook Hook, m Match) *Matcher {
	t.Helper()
	info, _ := Lookup(hook)
	c, err := m.Compile(info)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestMatcher(t *testing.T) {
	for _, c := range []struct {
		name string
		m    Match
		want bool
	}{
		{"empty", Match{}, true},
		{"in scope", Match{Scope: "in_scope"}, true},
		{"out of scope", Match{Scope: "out_of_scope"}, false},
		{"target", Match{Targets: []string{"t2", "t1"}}, true},
		{"subdomain", Match{Hosts: []string{"*.app.test"}}, true},
		{"exact host", Match{Hosts: []string{"app.test"}}, false},
		{"path glob", Match{Paths: []string{"/api/*"}}, true},
		{"path glob ignores query", Match{Paths: []string{"/api/v1/users"}}, true},
		{"path regex sees query", Match{PathRegex: `x=1$`}, true},
		{"method", Match{Methods: []string{"get"}}, false},
		{"status class", Match{Status: []string{"2xx"}}, true},
		{"status range", Match{Status: []string{"400-599"}}, false},
		{"content type suffix", Match{ContentTypes: []string{"application/*+json"}}, true},
		{"content type", Match{ContentTypes: []string{"text/html"}}, false},
		{"replay only", Match{Sources: []string{"replay"}}, false},
	} {
		if got := compile(t, TrafficOnResponseReceived, c.m).Match(flowFacts()); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}

	// Plugin traffic is left out unless asked for.
	f := flowFacts()
	f.Source = "plugin"
	if compile(t, TrafficOnFlowComplete, Match{}).Match(f) {
		t.Error("another plugin's traffic matched by default")
	}
	if !compile(t, TrafficOnFlowComplete, Match{Sources: []string{"plugin"}}).Match(f) {
		t.Error("plugin source not matched when asked for")
	}
	// No targets configured: everything is in scope.
	f = flowFacts()
	f.TargetID, f.NoTargets = "", true
	if !compile(t, TrafficOnFlowComplete, Match{Scope: "in_scope"}).Match(f) {
		t.Error("in_scope with no targets configured")
	}
	// Findings filter on type and source.
	fm := compile(t, FindingOnCreated, Match{Types: []string{"credential"}, Sources: []string{"plugin"}})
	if !fm.Match(&Facts{Source: "plugin", Types: []string{"x", "credential"}}) || fm.Match(&Facts{Source: "user", Types: []string{"credential"}}) {
		t.Error("finding matcher")
	}

	// A recovered file filters on its kind, its media type, its path —
	// relative, so no leading slash — and who recovered it.
	file := func() *Facts {
		return &Facts{Source: "plugin", Host: "app.test", Path: "app.test/main/src/api.ts",
			ContentType: "text/typescript", Types: []string{"source-map"}}
	}
	for _, c := range []struct {
		name string
		m    Match
		want bool
	}{
		{"empty", Match{}, true},
		{"kind", Match{Types: []string{"source-map"}}, true},
		{"other kind", Match{Types: []string{"source-map-dependency"}}, false},
		{"text", Match{ContentTypes: []string{"text/*"}}, true},
		{"not a font", Match{ContentTypes: []string{"font/*"}}, false},
		{"relative path glob", Match{Paths: []string{"*.ts"}}, true},
		{"under a host", Match{Paths: []string{"app.test/*"}}, true},
		{"another tree", Match{Paths: []string{"other.test/*"}}, false},
		{"host", Match{Hosts: []string{"app.test"}}, true},
		{"source", Match{Sources: []string{"user"}}, false},
	} {
		if got := compile(t, FileOnCreated, c.m).Match(file()); got != c.want {
			t.Errorf("file %s: %v, want %v", c.name, got, c.want)
		}
	}
	// A batch has no path or media type of its own, so a filter on either
	// says nothing about the recovery; what it does have is matched.
	batch := &Facts{Source: "plugin", Host: "app.test", Types: []string{"source-map"}}
	if !compile(t, FileOnBatchComplete, Match{Paths: []string{"*.ts"}, ContentTypes: []string{"text/*"}}).WantsFileBatch(batch) {
		t.Error("a batch was turned away by a filter about its files")
	}
	if compile(t, FileOnBatchComplete, Match{Hosts: []string{"other.test"}}).WantsFileBatch(batch) {
		t.Error("a batch from another host matched")
	}
}

// self says "tell me about my own work too". It is for traffic a plugin
// sent and files it recovered; nothing else distinguishes a plugin's own.
func TestSelfAppliesToTrafficAndFiles(t *testing.T) {
	for _, c := range []struct {
		hook Hook
		ok   bool
	}{
		{TrafficOnResponseReceived, true},
		{FileOnCreated, true},
		{FileOnBatchComplete, true},
		{EntityOnCreated, false},
		{FindingOnCreated, false},
	} {
		man := `{"name": "pp", "version": "1.0", "author": {"name": "a"}, "status": "enabled",
			"targets": [{"hook": "` + string(c.hook) + `", "function": "f", "self": true}]}`
		_, err := Parse([]byte(man), "pp")
		if c.ok && err != nil {
			t.Errorf("%s: %v", c.hook, err)
		}
		if !c.ok && (err == nil || !strings.Contains(err.Error(), "self only applies")) {
			t.Errorf("%s: err = %v", c.hook, err)
		}
	}
}

func TestContentType(t *testing.T) {
	for raw, want := range map[string]string{
		"HTTP/1.1 200 OK\r\nContent-Type: text/html\r\n\r\n":                "text/html",
		"HTTP/1.1 200 OK\r\nX: y\r\n\r\nContent-Type: text/html\r\n":        "",
		"GET / HTTP/1.1\r\ncontent-type:application/json;charset=x\r\n\r\n": "application/json",
	} {
		if got := ContentType([]byte(raw)); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", raw, got, want)
		}
	}
}

func BenchmarkMatcherMiss(b *testing.B) {
	info, _ := Lookup(TrafficOnResponseReceived)
	c, _ := (&Match{Scope: "in_scope", Hosts: []string{"*.other.test"}, ContentTypes: []string{"text/*"}}).Compile(info)
	f := flowFacts()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		c.Match(f)
	}
}

func TestParseActions(t *testing.T) {
	ok := `{
		"name": "acts", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"actions": [
			{"id": "retry-as-admin", "label": "Retry as admin", "function": "retry", "match": {"methods": ["GET"]}},
			{"id": "decode", "label": "Decode the token", "description": "Shows its claims", "function": "decode"}
		]
	}`
	m, err := Parse([]byte(ok), "acts")
	if err != nil {
		t.Fatalf("a plugin of actions alone: %v", err)
	}
	if len(m.Actions) != 2 || m.Actions[1].Description != "Shows its claims" {
		t.Fatalf("actions = %+v", m.Actions)
	}

	bad := `{
		"name": "acts", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"actions": [
			{"id": "Bad Id", "label": "", "function": "not a name"},
			{"id": "dup", "label": "One", "function": "f"},
			{"id": "dup", "label": "Two\nlines", "function": "f", "match": {"status": ["9xx"], "types": ["host"]}}
		]
	}`
	_, err = Parse([]byte(bad), "acts")
	if err == nil {
		t.Fatal("no error")
	}
	for _, want := range []string{
		"actions[0] (Bad Id): id must be",
		"actions[0] (Bad Id): label is required",
		"actions[0] (Bad Id): function \"not a name\"",
		`actions[2]: id "dup" is used twice`,
		"actions[2] (dup): label must be one line",
		"actions[2] (dup): match: status",
		"types doesn't apply",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}

	none := `{"name": "acts", "version": "1.0", "author": {"name": "t"}, "status": "enabled"}`
	if _, err := Parse([]byte(none), "acts"); err == nil || !strings.Contains(err.Error(), "or actions or commands must offer one") {
		t.Errorf("neither targets, actions nor commands: %v", err)
	}
}

func TestActionMatchesEverySourceByDefault(t *testing.T) {
	m, err := (*Match)(nil).CompileAction()
	if err != nil {
		t.Fatal(err)
	}
	if !m.Match(&Facts{Source: "plugin", Method: "GET"}) {
		t.Error("an action with no filter should be offered on a plugin's flow")
	}
	m, _ = (&Match{Sources: []string{"proxy"}}).CompileAction()
	if m.Match(&Facts{Source: "plugin"}) {
		t.Error("sources given should still narrow it")
	}
}

func TestParseCommands(t *testing.T) {
	src := `{
		"name": "cmds", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"commands": [
			{"action": "run", "summary": "fuzz it", "function": "run",
			 "args": "<file>",
			 "flags": [
				{"name": "word", "default": "x"},
				{"name": "times", "type": "number", "default": 3},
				{"name": "loud", "type": "boolean"},
				{"name": "tags", "type": "list", "default": ["a", "b"]}
			 ]}
		]
	}`
	m, err := Parse([]byte(src), "cmds")
	if err != nil {
		t.Fatal(err)
	}
	c := m.Commands[0]
	if c.Action != "run" || c.Function != "run" || c.Args != "<file>" {
		t.Errorf("command is %+v", c)
	}
	// A type is inferred from the default, as a setting's is.
	if c.Flags[0].Type != FlagString || c.Flags[1].Type != FlagNumber || c.Flags[3].Type != FlagList {
		t.Errorf("types are %q %q %q", c.Flags[0].Type, c.Flags[1].Type, c.Flags[3].Type)
	}
	for i, want := range []string{"x", "3", "", "a,b"} {
		if got := c.Flags[i].DefaultString(); got != want {
			t.Errorf("flags[%d] default %q, want %q", i, got, want)
		}
	}
	// A plugin's commands answer to the plugin's own name.
	if ns := m.Namespace(); ns != "cmds" {
		t.Errorf("namespace is %q, want the plugin's name", ns)
	}
}

func TestParseCommandProblems(t *testing.T) {
	src := `{
		"name": "cmds", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"commands": [
			{"action": "", "function": "9", "summary": ""},
			{"action": "run", "summary": "a", "function": "run",
			 "flags": [
				{"name": "json"},
				{"name": "ok", "type": "secret"},
				{"name": "n", "type": "number", "default": "lots"},
				{"name": "dup"}, {"name": "dup"}
			 ]},
			{"action": "run", "summary": "a", "function": "run"}
		]
	}`
	_, err := Parse([]byte(src), "cmds")
	if err == nil {
		t.Fatal("a manifest full of bad commands was accepted")
	}
	for _, want := range []string{
		"action \"\"", "function \"9\"", "summary is required",
		"--json is a global flag", "type \"secret\"", "default:", "--dup is declared twice",
		"the action \"run\" is declared twice",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing %q in:\n%v", want, err)
		}
	}
}
