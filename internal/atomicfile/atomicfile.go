// Package atomicfile writes a file in one step: a temp file beside it,
// then a rename, so a crash mid-write never leaves a truncated file and a
// reader never sees a partial one.
//
// Every caller states the mode it wants. The temp file is chmod'ed to it
// before anything is written, so the file is never briefly readable at a
// wider mode than it ends up at — and so a private file is private by the
// code saying so, not by os.CreateTemp happening to create at 0600. That
// matters here: reproxy's files hold captured traffic, harvested findings
// and API keys.
package atomicfile

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Private is the mode for a file only its owner may read: secrets, API
// keys, captured traffic, a CA's private key.
const Private fs.FileMode = 0o600

// PrivateDir is the mode for a directory only its owner may enter, for
// anything holding Private files.
const PrivateDir fs.FileMode = 0o700

// Write writes data to path with mode, creating path's parent directory
// (and any missing parents) at dirMode if it does not exist.
//
// The rename is atomic within one filesystem, which holds because the temp
// file is made in path's own directory.
func Write(path string, data []byte, mode, dirMode fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, dirMode); err != nil {
		return fmt.Errorf("creating %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+"-*")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	defer os.Remove(tmp.Name()) // a no-op once the rename has happened

	// Before the write, so the content is never visible at a wider mode.
	// Some filesystems (and Windows) have no chmod; there the mode is the
	// platform's business and not an error.
	if err := tmp.Chmod(mode); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return nil
}

// WriteJSON marshals v as indented JSON, with a trailing newline, and
// writes it with Write.
func WriteJSON(path string, v any, mode, dirMode fs.FileMode) error {
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}
	return Write(path, append(data, '\n'), mode, dirMode)
}
