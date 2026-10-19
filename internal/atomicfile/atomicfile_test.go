package atomicfile

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestWriteCreatesTheFileAndItsParents(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a", "b", "file.txt")
	if err := Write(path, []byte("hello"), 0o644, 0o755); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "hello" {
		t.Errorf("content = %q", got)
	}
}

func TestWriteUsesTheModeItIsGiven(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	dir := t.TempDir()
	for _, tc := range []struct{ mode, dirMode fs.FileMode }{
		{Private, PrivateDir},
		{0o644, 0o755},
	} {
		sub := filepath.Join(dir, tc.mode.String())
		path := filepath.Join(sub, "f")
		if err := Write(path, []byte("x"), tc.mode, tc.dirMode); err != nil {
			t.Fatal(err)
		}
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != tc.mode {
			t.Errorf("file mode = %v, want %v", fi.Mode().Perm(), tc.mode)
		}
		di, err := os.Stat(sub)
		if err != nil {
			t.Fatal(err)
		}
		if di.Mode().Perm() != tc.dirMode {
			t.Errorf("dir mode = %v, want %v", di.Mode().Perm(), tc.dirMode)
		}
	}
}

// TestWriteDoesNotWidenAnExistingFile is the property the old copies got
// only by accident: a file written at Private must not come back readable
// by anyone else, whatever os.CreateTemp defaults to.
func TestWriteDoesNotWidenAnExistingFile(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no POSIX modes")
	}
	path := filepath.Join(t.TempDir(), "secrets.json")
	if err := Write(path, []byte("{}"), 0o644, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := Write(path, []byte("{}"), Private, 0o755); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != Private {
		t.Errorf("rewriting at Private left the file %v", fi.Mode().Perm())
	}
}

// TestWriteLeavesNoTempFileBehind covers both paths: success (renamed) and
// failure (removed).
func TestWriteLeavesNoTempFileBehind(t *testing.T) {
	dir := t.TempDir()
	if err := Write(filepath.Join(dir, "ok"), []byte("x"), 0o644, 0o755); err != nil {
		t.Fatal(err)
	}
	// A path whose "parent" is an existing file cannot be written.
	if err := Write(filepath.Join(dir, "ok", "nested"), []byte("x"), 0o644, 0o755); err == nil {
		t.Error("expected an error writing under a regular file")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".") {
			t.Errorf("temp file left behind: %s", e.Name())
		}
	}
}

// TestWriteReplacesAtomically checks the point of the package: a reader
// either sees the old content or the new, never a truncated file.
func TestWriteReplacesAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f")
	old := strings.Repeat("a", 4096)
	if err := Write(path, []byte(old), 0o644, 0o755); err != nil {
		t.Fatal(err)
	}
	// Hold the old file open; the rename must not disturb it.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := Write(path, []byte(strings.Repeat("b", 8192)), 0o644, 0o755); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, len(old))
	if _, err := f.Read(buf); err != nil {
		t.Fatal(err)
	}
	if string(buf) != old {
		t.Error("the open handle saw the replacement, so the write was not atomic")
	}
}

func TestWriteJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	in := map[string]any{"b": 2, "a": 1}
	if err := WriteJSON(path, in, Private, PrivateDir); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(string(data), "\n") {
		t.Error("no trailing newline")
	}
	if !strings.Contains(string(data), "\n  ") {
		t.Errorf("not indented: %q", data)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("not valid JSON: %v", err)
	}
	if len(out) != 2 {
		t.Errorf("round trip = %v", out)
	}
}

func TestWriteJSONReportsAMarshalFailure(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.json")
	err := WriteJSON(path, func() {}, Private, PrivateDir)
	if err == nil {
		t.Fatal("expected an error for an unmarshalable value")
	}
	if _, statErr := os.Stat(path); !errors.Is(statErr, fs.ErrNotExist) {
		t.Error("a failed marshal still created the file")
	}
}
