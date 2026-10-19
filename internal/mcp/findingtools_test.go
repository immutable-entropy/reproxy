package mcp

import (
	"strings"
	"testing"

	"reproxy/internal/config"
	"reproxy/internal/findings"
)

type findingsOut struct {
	Findings []struct {
		Handle string   `json:"handle"`
		Value  string   `json:"value"`
		Types  []string `json:"types"`
		Notes  string   `json:"notes"`
		Source string   `json:"source"`
		Author string   `json:"author"`
		Viewed bool     `json:"viewed"`
		Seen   []struct {
			Flow     string `json:"flow"`
			Method   string `json:"method"`
			URL      string `json:"url"`
			Location string `json:"location"`
			Detail   string `json:"detail"`
		} `json:"seen"`
	} `json:"findings"`
	Matched int  `json:"matched"`
	More    bool `json:"more"`
}

type createOut struct {
	Finding struct {
		Handle string   `json:"handle"`
		Value  string   `json:"value"`
		Types  []string `json:"types"`
		Notes  string   `json:"notes"`
		Fields []struct {
			Key   string `json:"key"`
			Value string `json:"value"`
		} `json:"fields"`
		Source     string `json:"source"`
		Author     string `json:"author"`
		Viewed     bool   `json:"viewed"`
		Confidence *int   `json:"confidence"`
		Concern    *int   `json:"concern"`
		Seen       []struct {
			Flow     string `json:"flow"`
			Location string `json:"location"`
		} `json:"seen"`
	} `json:"finding"`
	Created bool `json:"created"`
	Merged  bool `json:"merged"`
}

func TestCreateFindingRecordsTheFlowItWasSeenIn(t *testing.T) {
	f := newFixture(t)
	id := f.addFlow("GET", "a.test", "/me", "")

	var out createOut
	if err := f.call(t, auto, "create_finding", map[string]any{
		"note":     "Static AWS key returned to unauthenticated callers.",
		"value":    "AKIAQ7",
		"types":    []string{"Credential"}, // by name: the model shouldn't have to guess slugs
		"fields":   map[string]string{"account": "4711"},
		"flow":     flowHandle(testInstance, id),
		"location": findings.InResponseBody,
		"detail":   "line 42",
	}, &out); err != nil {
		t.Fatal(err)
	}
	switch {
	case !out.Created || out.Merged:
		t.Errorf("created=%v merged=%v", out.Created, out.Merged)
	case out.Finding.Handle != findingHandle(testInstance, 1):
		t.Errorf("handle = %q", out.Finding.Handle)
	case strings.Join(out.Finding.Types, ",") != "credential":
		t.Errorf("types = %v: a name should resolve to its id", out.Finding.Types)
	case out.Finding.Source != string(findings.SourceAgent) || out.Finding.Viewed:
		t.Errorf("source = %q, viewed = %v: an agent's finding arrives unviewed", out.Finding.Source, out.Finding.Viewed)
	case len(out.Finding.Fields) != 1 || out.Finding.Fields[0].Key != "account":
		t.Errorf("fields = %+v", out.Finding.Fields)
	case len(out.Finding.Seen) != 1 || out.Finding.Seen[0].Flow != flowHandle(testInstance, id):
		t.Errorf("seen = %+v", out.Finding.Seen)
	}

	// The sighting carries the flow's method and URL, so the finding still
	// reads sensibly once the traffic has been cleared.
	var list findingsOut
	if err := f.call(t, auto, "list_findings", nil, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Findings) != 1 || list.Findings[0].Seen[0].URL != "http://a.test/me" {
		t.Fatalf("list_findings = %+v", list.Findings)
	}
}

// The same value found again is one finding with two sightings: that is what
// keeps an agent sweeping an API from filling the list with one credential.
func TestCreateFindingMergesTheSameValue(t *testing.T) {
	f := newFixture(t)
	one := f.addFlow("GET", "a.test", "/me", "")
	two := f.addFlow("GET", "a.test", "/orders", "")

	for _, flow := range []uint64{one, two} {
		var out createOut
		if err := f.call(t, auto, "create_finding", map[string]any{
			"note":  "Key echoed in the response.",
			"value": "AKIAQ7",
			"types": []string{"credential"},
			"flow":  flowHandle(testInstance, flow),
		}, &out); err != nil {
			t.Fatal(err)
		}
		if flow == two && (out.Created || !out.Merged) {
			t.Errorf("second create: created=%v merged=%v", out.Created, out.Merged)
		}
	}
	var list findingsOut
	if err := f.call(t, auto, "list_findings", nil, &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Findings) != 1 {
		t.Fatalf("%d findings, want 1", len(list.Findings))
	}
	if len(list.Findings[0].Seen) != 2 {
		t.Errorf("sightings = %+v", list.Findings[0].Seen)
	}
}

// An agent that records a finding has to say what it is: a bare value in a
// list nobody can interpret is worse than nothing.
func TestCreateFindingRefusesABlankNote(t *testing.T) {
	f := newFixture(t)
	err := f.call(t, auto, "create_finding", map[string]any{"value": "AKIAQ7", "note": "   "}, nil)
	if err == nil {
		t.Fatal("a finding with a blank note was accepted")
	}
	if !strings.Contains(err.Error(), "note") {
		t.Errorf("error = %v; it should tell the model what to fix", err)
	}
	// The schema requires the field as well, so leaving it out fails too.
	if err := f.call(t, auto, "create_finding", map[string]any{"value": "AKIAQ7"}, nil); err == nil {
		t.Error("a finding with no note at all was accepted")
	}
}

func TestCreateFindingRejectsAnUnknownTypeAndListsTheRealOnes(t *testing.T) {
	f := newFixture(t)
	err := f.call(t, auto, "create_finding", map[string]any{
		"note": "Something.", "value": "x", "types": []string{"nonsense"},
	}, nil)
	if err == nil {
		t.Fatal("an unknown type was accepted")
	}
	if !strings.Contains(err.Error(), "credential") {
		t.Errorf("error = %v; it should list the configured types", err)
	}
}

// Findings belong to one process's engagement, so a flow from elsewhere has
// nowhere to be recorded and the tool says so rather than dropping the link.
func TestCreateFindingRefusesAnotherInstancesFlow(t *testing.T) {
	f := newFixture(t)
	err := f.call(t, auto, "create_finding", map[string]any{
		"note": "Something.", "value": "x", "flow": flowHandle("deadbeef", 1),
	}, nil)
	if err == nil {
		t.Fatal("a flow handle from another instance was accepted")
	}
	if !strings.Contains(err.Error(), "deadbeef") {
		t.Errorf("error = %v", err)
	}
}

// Recording a finding changes what the engagement holds, so a read-only
// client may not do it — but it costs no approval prompt for anyone else.
func TestCreateFindingIsRefusedForReadOnlyClients(t *testing.T) {
	f := newFixture(t)
	err := f.call(t, Caller{Policy: config.PolicyReadOnly, Client: "reader"}, "create_finding",
		map[string]any{"note": "Something.", "value": "x"}, nil)
	if err != ErrReadOnly {
		t.Fatalf("read-only create_finding: %v, want ErrReadOnly", err)
	}

	// Ask mode runs it without going anywhere near the broker: a finding
	// sends nothing and changes no traffic.
	var out createOut
	if err := f.call(t, Caller{Policy: config.PolicyAsk, Client: "asker"}, "create_finding",
		map[string]any{"note": "Something.", "value": "x"}, &out); err != nil {
		t.Fatalf("ask-mode create_finding: %v", err)
	}
	if !out.Created {
		t.Error("ask-mode create_finding recorded nothing")
	}
	if n := len(f.broker.Pending()); n != 0 {
		t.Errorf("%d approvals pending; recording a finding must not interrupt the user", n)
	}
	if out.Finding.Author != "asker" {
		t.Errorf("author = %q, want the client's name", out.Finding.Author)
	}
}

func TestListFindingsFilters(t *testing.T) {
	f := newFixture(t)
	id := f.addFlow("GET", "a.test", "/users", "")
	for _, args := range []map[string]any{
		{"note": "Key in the bundle.", "value": "AKIAQ7", "types": []string{"credential"}},
		{"note": "PII in the list response.", "value": "bob@a.test",
			"types": []string{"sensitive-data"}, "flow": flowHandle(testInstance, id)},
	} {
		if err := f.call(t, auto, "create_finding", args, nil); err != nil {
			t.Fatal(err)
		}
	}

	for name, probe := range map[string]struct {
		args map[string]any
		want int
	}{
		"everything":   {nil, 2},
		"by type":      {map[string]any{"types": []string{"credential"}}, 1},
		"by text":      {map[string]any{"query": "PII"}, 1},
		"by regex":     {map[string]any{"query": "^bob@", "regex": true}, 1},
		"by flow":      {map[string]any{"flow": flowHandle(testInstance, id)}, 1},
		"unviewed":     {map[string]any{"unviewed": true}, 2},
		"limit of one": {map[string]any{"limit": 1}, 1},
	} {
		var out findingsOut
		if err := f.call(t, auto, "list_findings", probe.args, &out); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if len(out.Findings) != probe.want {
			t.Errorf("%s: %d findings, want %d", name, len(out.Findings), probe.want)
		}
	}

	var limited findingsOut
	if err := f.call(t, auto, "list_findings", map[string]any{"limit": 1}, &limited); err != nil {
		t.Fatal(err)
	}
	if limited.Matched != 2 || !limited.More {
		t.Errorf("matched=%d more=%v; a capped page should say there is more", limited.Matched, limited.More)
	}
}

func TestListFindingTypesIsTheUsersCatalog(t *testing.T) {
	f := newFixture(t)
	var out struct {
		Types []findings.Type `json:"types"`
	}
	if err := f.call(t, auto, "list_finding_types", nil, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Types) != len(testFindingTypes) || out.Types[0].ID != "credential" || out.Types[0].Color == "" {
		t.Fatalf("types = %+v", out.Types)
	}
}

// A process with no findings store (no engagement machinery at all) must
// answer rather than panic.
func TestFindingToolsWithoutAStore(t *testing.T) {
	f := newFixture(t)
	f.reg.findings = nil
	for _, name := range []string{"list_finding_types", "list_findings"} {
		if err := f.call(t, auto, name, nil, nil); err != ErrNoFindings {
			t.Errorf("%s = %v, want ErrNoFindings", name, err)
		}
	}
	if err := f.call(t, auto, "create_finding", map[string]any{"note": "x", "value": "y"}, nil); err != ErrNoFindings {
		t.Errorf("create_finding = %v, want ErrNoFindings", err)
	}
}

func TestSetFindingConfidenceAndFilter(t *testing.T) {
	f := newFixture(t)

	var created createOut
	if err := f.call(t, auto, "create_finding", map[string]any{
		"note": "a stack banner, probably noise", "value": "X-Powered-By: PHP/8.1",
	}, &created); err != nil {
		t.Fatal(err)
	}
	handle := created.Finding.Handle
	// A new finding is unreviewed: no score yet.
	if created.Finding.Confidence != nil {
		t.Fatalf("new finding confidence = %v, want null (unreviewed)", *created.Finding.Confidence)
	}

	// An unreviewed finding is listed with unreviewed:true and is not hidden
	// by the floor (it has no score).
	var unrev findingsOut
	if err := f.call(t, auto, "list_findings", map[string]any{"unreviewed": true}, &unrev); err != nil {
		t.Fatal(err)
	}
	if len(unrev.Findings) != 1 {
		t.Errorf("unreviewed:true didn't return the unscored finding: %+v", unrev.Findings)
	}
	var beforeFilter findingsOut
	if err := f.call(t, auto, "list_findings", map[string]any{"min_confidence": 40}, &beforeFilter); err != nil {
		t.Fatal(err)
	}
	if len(beforeFilter.Findings) != 1 {
		t.Errorf("an unreviewed finding was hidden by min_confidence 40: %+v", beforeFilter.Findings)
	}

	// Score it down as likely noise.
	var scored struct {
		Finding struct {
			Confidence *int `json:"confidence"`
		} `json:"finding"`
	}
	if err := f.call(t, auto, "set_finding_confidence", map[string]any{"finding": handle, "confidence": 15}, &scored); err != nil {
		t.Fatal(err)
	}
	if scored.Finding.Confidence == nil || *scored.Finding.Confidence != 15 {
		t.Fatalf("confidence = %v, want 15", scored.Finding.Confidence)
	}

	// Now it's hidden when the list filters at the floor, present otherwise.
	var filtered, all findingsOut
	if err := f.call(t, auto, "list_findings", map[string]any{"min_confidence": 40}, &filtered); err != nil {
		t.Fatal(err)
	}
	if len(filtered.Findings) != 0 {
		t.Errorf("a low-confidence finding showed with min_confidence 40: %+v", filtered.Findings)
	}
	if err := f.call(t, auto, "list_findings", nil, &all); err != nil {
		t.Fatal(err)
	}
	if len(all.Findings) != 1 {
		t.Errorf("the low-confidence finding was dropped from the default list: %+v", all.Findings)
	}
	// Once scored, it is no longer unreviewed.
	var stillUnrev findingsOut
	if err := f.call(t, auto, "list_findings", map[string]any{"unreviewed": true}, &stillUnrev); err != nil {
		t.Fatal(err)
	}
	if len(stillUnrev.Findings) != 0 {
		t.Errorf("a scored finding still showed as unreviewed: %+v", stillUnrev.Findings)
	}

	// Scoring it back up brings it into the filtered list.
	if err := f.call(t, auto, "set_finding_confidence", map[string]any{"finding": handle, "confidence": 85}, nil); err != nil {
		t.Fatal(err)
	}
	var again findingsOut
	if err := f.call(t, auto, "list_findings", map[string]any{"min_confidence": 40}, &again); err != nil {
		t.Fatal(err)
	}
	if len(again.Findings) != 1 {
		t.Errorf("scoring it up didn't restore the finding: %+v", again.Findings)
	}

	// Confidence is required.
	if err := f.call(t, auto, "set_finding_confidence", map[string]any{"finding": handle}, nil); err == nil {
		t.Error("set_finding_confidence without a score was accepted")
	}

	// A read-only caller may not score.
	ro := Caller{Policy: config.PolicyReadOnly}
	if err := f.call(t, ro, "set_finding_confidence", map[string]any{"finding": handle, "confidence": 50}, nil); err != ErrReadOnly {
		t.Fatalf("read-only score = %v, want ErrReadOnly", err)
	}
}

func TestSetFindingConcernAutoReads(t *testing.T) {
	f := newFixture(t)

	var created createOut
	if err := f.call(t, auto, "create_finding", map[string]any{
		"note": "a stack banner", "value": "X-Powered-By: PHP/8.1",
	}, &created); err != nil {
		t.Fatal(err)
	}
	handle := created.Finding.Handle
	if created.Finding.Concern != nil {
		t.Fatalf("new finding concern = %v, want null (unassessed)", *created.Finding.Concern)
	}

	// Score it low on confidence; not yet read (concern unknown).
	if err := f.call(t, auto, "set_finding_confidence", map[string]any{"finding": handle, "confidence": 20}, nil); err != nil {
		t.Fatal(err)
	}

	// Assess concern, also low: now both are below 50, so it is auto-read.
	var scored struct {
		Finding struct {
			Concern *int `json:"concern"`
			Viewed  bool `json:"viewed"`
		} `json:"finding"`
	}
	if err := f.call(t, auto, "set_finding_concern", map[string]any{"finding": handle, "concern": 10}, &scored); err != nil {
		t.Fatal(err)
	}
	if scored.Finding.Concern == nil || *scored.Finding.Concern != 10 {
		t.Fatalf("concern = %v, want 10", scored.Finding.Concern)
	}
	if !scored.Finding.Viewed {
		t.Error("a both-low finding wasn't auto-read")
	}

	// Concern is required.
	if err := f.call(t, auto, "set_finding_concern", map[string]any{"finding": handle}, nil); err == nil {
		t.Error("set_finding_concern without a score was accepted")
	}

	// A read-only caller may not score.
	ro := Caller{Policy: config.PolicyReadOnly}
	if err := f.call(t, ro, "set_finding_concern", map[string]any{"finding": handle, "concern": 50}, nil); err != ErrReadOnly {
		t.Fatalf("read-only score = %v, want ErrReadOnly", err)
	}
}
