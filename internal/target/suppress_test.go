package target

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestSuppressorMatches(t *testing.T) {
	s := CompileSuppressions([]Suppression{
		{Pattern: "Fonts.GStatic.com"}, {Pattern: "*.doubleclick.net"}, {Pattern: "10.0.0.0/8"}, {Pattern: "192.0.2.7"},
	})
	for host, want := range map[string]bool{
		"fonts.gstatic.com": true, "fonts.gstatic.com:443": true, "gstatic.com": false, "x.fonts.gstatic.com": false,
		"ad.doubleclick.net": true, "a.b.doubleclick.net": true, "doubleclick.net": false,
		"10.1.2.3": true, "11.0.0.1": false, "192.0.2.7": true, "[::1]:80": false,
	} {
		if got := s.Suppressed(host); got != want {
			t.Errorf("Suppressed(%q) = %v, want %v", host, got, want)
		}
	}
	var none *Suppressor
	if none.Suppressed("x.test") {
		t.Error("nil suppressor matched")
	}
}

func TestValidateSuppressions(t *testing.T) {
	if err := ValidateSuppressions([]Suppression{{Pattern: "a.test"}, {Pattern: "10.0.0.0/8"}}); err != nil {
		t.Error(err)
	}
	for _, bad := range [][]Suppression{
		{{Pattern: ""}}, {{Pattern: "10.0.0.0/99"}}, {{Pattern: "a.test"}, {Pattern: "A.test "}},
	} {
		if ValidateSuppressions(bad) == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}

func TestSuppressionListRoundTrip(t *testing.T) {
	at := time.Unix(100, 0)
	list := []Suppression{{Pattern: "*.doubleclick.net", Note: "ads"}, {Pattern: "10.0.0.0/8"}}
	var buf bytes.Buffer
	if err := ExportSuppressions(&buf, list); err != nil {
		t.Fatal(err)
	}
	got, errs := ParseSuppressions(&buf, at)
	if len(errs) != 0 {
		t.Fatal(errs)
	}
	want := []Suppression{{Pattern: "*.doubleclick.net", Note: "ads", Added: at}, {Pattern: "10.0.0.0/8", Added: at}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseSuppressionsSkipsBadLines(t *testing.T) {
	in := "# header\n\nGood.Test  # a note\n10.0.0.0/99\ntwo words\ngood.test\n"
	got, errs := ParseSuppressions(strings.NewReader(in), time.Time{})
	if len(got) != 1 || got[0].Pattern != "good.test" || got[0].Note != "a note" {
		t.Errorf("got %+v", got)
	}
	if len(errs) != 2 || !strings.Contains(errs[0].Error(), "line 4") || !strings.Contains(errs[1].Error(), "line 5") {
		t.Errorf("errs = %v", errs)
	}
}

func TestMergeSuppressions(t *testing.T) {
	list, n := MergeSuppressions([]Suppression{{Pattern: "a.test"}}, []Suppression{{Pattern: "A.test"}, {Pattern: "b.test"}})
	if n != 1 || len(list) != 2 || list[1].Pattern != "b.test" {
		t.Errorf("got %+v, %d", list, n)
	}
}
