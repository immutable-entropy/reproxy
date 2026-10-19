package mcp

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"reproxy/internal/files"
)

type filesOut struct {
	Files []struct {
		ID        uint64 `json:"id"`
		Path      string `json:"path"`
		Bytes     int64  `json:"bytes"`
		MediaType string `json:"media_type"`
		Text      bool   `json:"text"`
		Kind      string `json:"kind"`
		Source    string `json:"source"`
		Author    string `json:"author"`
		Batch     string `json:"batch"`
		Flow      string `json:"flow"`
		Host      string `json:"host"`
	} `json:"files"`
	Matched   int    `json:"matched"`
	More      bool   `json:"more"`
	Directory string `json:"directory"`
}

type fileOutResult struct {
	File struct {
		ID        uint64 `json:"id"`
		Path      string `json:"path"`
		MediaType string `json:"media_type"`
		Flow      string `json:"flow"`
	} `json:"file"`
	Text      *string `json:"text"`
	Truncated bool    `json:"truncated"`
	Note      string  `json:"note"`
}

// recover puts a file in the fixture's engagement as a plugin's recovery.
func (f *fixture) recover(t *testing.T, path, content string, d files.Draft) files.File {
	t.Helper()
	d.Path, d.Content = path, []byte(content)
	if d.Source == "" {
		d.Source, d.Author = files.SourcePlugin, "spa-recon"
	}
	out, _, err := f.files.Record(d)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestFileToolsReadWhatWasRecovered(t *testing.T) {
	f := newFixture(t)
	id := f.addFlow("GET", "app.test", "/assets/main.js.map", "")
	src := f.recover(t, "app.test/main/src/api.ts", "const key = \"sk-live-1\";", files.Draft{
		Kind: files.KindSourceMap, FlowID: id, Method: "GET",
		URL: "https://app.test/assets/main.js.map", Host: "app.test", Batch: "b1",
	})
	f.recover(t, "app.test/main/fonts/x.woff2", "\x00\x01binary", files.Draft{
		Kind: files.KindSourceMap, Host: "app.test", Batch: "b1",
	})

	var list filesOut
	if err := f.call(t, Caller{}, "list_files", map[string]any{}, &list); err != nil {
		t.Fatal(err)
	}
	if list.Matched != 2 || len(list.Files) != 2 {
		t.Fatalf("list = %+v", list)
	}
	// Newest first, and every file says where it came from.
	if list.Files[1].Path != "app.test/main/src/api.ts" || list.Files[1].MediaType != "text/typescript" ||
		!list.Files[1].Text || list.Files[1].Author != "spa-recon" || list.Files[1].Flow == "" {
		t.Errorf("file = %+v", list.Files[1])
	}

	// A text-only listing leaves the font out, which is what a scanner asks for.
	var text filesOut
	if err := f.call(t, Caller{}, "list_files", map[string]any{"text_only": true}, &text); err != nil {
		t.Fatal(err)
	}
	if text.Matched != 1 || text.Files[0].Path != "app.test/main/src/api.ts" {
		t.Errorf("text-only list = %+v", text)
	}

	// By flow handle, as the model would pass it back.
	var byFlow filesOut
	if err := f.call(t, Caller{}, "list_files",
		map[string]any{"flow": flowHandle(testInstance, id)}, &byFlow); err != nil {
		t.Fatal(err)
	}
	if byFlow.Matched != 1 {
		t.Errorf("by flow = %+v", byFlow)
	}

	// get_file hands back the text.
	var got fileOutResult
	if err := f.call(t, Caller{}, "get_file", map[string]any{"id": src.ID}, &got); err != nil {
		t.Fatal(err)
	}
	if got.Text == nil || !strings.Contains(*got.Text, "sk-live-1") || got.Truncated {
		t.Fatalf("get_file = %+v", got)
	}
	// By path, bounded, and the result says it was cut short.
	var cut fileOutResult
	if err := f.call(t, Caller{}, "get_file",
		map[string]any{"path": "app.test/main/src/api.ts", "max_bytes": 5}, &cut); err != nil {
		t.Fatal(err)
	}
	if cut.Text == nil || *cut.Text != "const" || !cut.Truncated {
		t.Fatalf("bounded get_file = %+v", cut)
	}
	// A file that isn't text is described, not returned.
	var font fileOutResult
	if err := f.call(t, Caller{}, "get_file",
		map[string]any{"path": "app.test/main/fonts/x.woff2"}, &font); err != nil {
		t.Fatal(err)
	}
	if font.Text != nil || !strings.Contains(font.Note, "not text") {
		t.Fatalf("a font = %+v", font)
	}
	// And a file nobody recovered says so.
	if err := f.call(t, Caller{}, "get_file", map[string]any{"id": 99}, nil); err == nil {
		t.Error("a missing file should be an error")
	}
	if err := f.call(t, Caller{}, "get_file", map[string]any{}, nil); err == nil {
		t.Error("get_file with neither id nor path should be an error")
	}
}

// get_file reads only what the store recorded under the recovered-files
// root: there is no way to name an arbitrary path on disk. A traversal or an
// absolute path resolves (through files.SafeRel) to something inside the
// root, which matches no recorded file, so the read is refused rather than
// reaching outside.
func TestGetFileCannotEscapeTheRecoveredRoot(t *testing.T) {
	f := newFixture(t)
	f.recover(t, "app.test/main/src/api.ts", "const key = 1;", files.Draft{Kind: files.KindSourceMap, Host: "app.test"})

	// A real secret outside the root, to be sure nothing hands it back.
	secret := filepath.Join(filepath.Dir(f.files.Root()), "outside-secret.txt")
	if err := os.WriteFile(secret, []byte("TOP SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{
		"../../../../etc/passwd",
		"/etc/passwd",
		"app.test/../../../../outside-secret.txt",
		"../outside-secret.txt",
		"webpack://../../outside-secret.txt",
		"C:\\Windows\\win.ini",
	} {
		var got fileOutResult
		err := f.call(t, Caller{}, "get_file", map[string]any{"path": path}, &got)
		if err == nil {
			t.Errorf("get_file(%q) was allowed: %+v", path, got)
		}
		if got.Text != nil && strings.Contains(*got.Text, "SECRET") {
			t.Fatalf("get_file(%q) read a file outside the recovered root", path)
		}
	}
}

// A process with no recovered-file store says so rather than panicking.
func TestFileToolsWithoutAStore(t *testing.T) {
	f := newFixture(t)
	f.reg.SetFiles(nil)
	if err := f.call(t, Caller{}, "list_files", map[string]any{}, nil); err == nil ||
		!strings.Contains(err.Error(), "recovered files") {
		t.Errorf("err = %v", err)
	}
}
