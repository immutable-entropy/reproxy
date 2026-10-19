// Package plugintest runs a plugin's unit tests: every *.test.ts in its
// directory, each in a runtime of its own with the plugin's main.ts, against
// a throwaway engagement — a temporary capture.db with real capture,
// findings and entities stores — so what a test checks is what the plugin
// would do in the app. Outbound requests are answered from mock.respond's
// script instead of being sent.
//
// It backs `reproxy plugin test`.
package plugintest

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"reproxy/internal/archive"
	"reproxy/internal/capture"
	"reproxy/internal/config"
	"reproxy/internal/entities"
	"reproxy/internal/files"
	"reproxy/internal/findings"
	"reproxy/internal/intercept"
	"reproxy/internal/plugin"
	"reproxy/internal/target"
)

// Options configure a run.
type Options struct {
	// Filter runs only the tests whose name matches this regular expression.
	Filter string
	// Log receives the plugin's log; nil discards it.
	Log io.Writer
}

// Report is how a plugin's tests went.
type Report struct {
	Results []plugin.TestResult
	// Errors are files that couldn't run at all.
	Errors []error
}

// Passed reports whether every test passed and every file ran.
func (r Report) Passed() bool {
	if len(r.Errors) > 0 {
		return false
	}
	for _, t := range r.Results {
		if !t.OK {
			return false
		}
	}
	return true
}

// Run runs a plugin's tests.
func Run(src plugin.Source, opts Options) (Report, error) {
	man, problems := plugin.Inspect(src)
	if len(problems) > 0 {
		return Report{}, fmt.Errorf("%s has problems:\n  %s", src.Name, strings.Join(problems, "\n  "))
	}
	testFiles := plugin.TestFiles(src.FS)
	if len(testFiles) == 0 {
		return Report{}, fmt.Errorf("%s has no tests (*.test.ts)", src.Name)
	}

	dir, err := os.MkdirTemp("", "reproxy-plugintest-")
	if err != nil {
		return Report{}, err
	}
	defer os.RemoveAll(dir)
	ar, err := archive.Open(filepath.Join(dir, archive.FileName))
	if err != nil {
		return Report{}, err
	}
	defer ar.Close()
	db := ar.DB()

	store := capture.NewStore()
	fs := findings.NewStore()
	if err := fs.SetTypes(config.Default().Findings.Types); err != nil {
		return Report{}, err
	}
	if err := fs.Use(db); err != nil {
		return Report{}, err
	}
	if len(man.FindingTypes) > 0 {
		types := make([]findings.Type, len(man.FindingTypes))
		for i, t := range man.FindingTypes {
			types[i] = findings.Type{ID: t.ID, Name: t.Name, Color: t.Color}
		}
		if err := fs.DeclareTypes(man.Name, types); err != nil {
			return Report{}, err
		}
	}
	ents := entities.NewStore()
	if err := ents.Use(db); err != nil {
		return Report{}, err
	}
	// The recovered files of the throwaway engagement: their records in its
	// archive, their bytes under the temporary directory, which goes with
	// it when the run ends.
	recovered := files.NewStore()
	if err := recovered.Use(db, filepath.Join(dir, "recovered")); err != nil {
		return Report{}, err
	}
	targets, err := target.NewStore(filepath.Join(dir, "targets.json"))
	if err != nil {
		return Report{}, err
	}
	// The Explorer sees the test's targets (mock.target) as the app's does,
	// so what a plugin adds can be Related.
	ents.SetScope(func(host string) (string, bool) {
		m, ok := targets.Set().MatchHost(host)
		return m.ID, ok
	})
	ic, err := intercept.New(intercept.Settings{})
	if err != nil {
		return Report{}, err
	}
	logOut := opts.Log
	if logOut == nil {
		logOut = io.Discard
	}
	deps := plugin.Deps{
		DataDir: dir, Store: store, Intercept: ic, Findings: fs, Targets: targets, Entities: ents,
		Files:        recovered,
		EngagementDB: ar.DB,
		Log:          slog.New(slog.NewTextHandler(logOut, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	reset := func() {
		store.Clear()
		for _, q := range []string{`DELETE FROM findings`, `DELETE FROM plugin_kv`} {
			_, _ = db.Exec(q)
		}
		_ = fs.Use(db)
		for _, t := range targets.List() {
			_ = targets.Remove(t.ID)
		}
		_ = ents.Clear()
		_ = recovered.Clear()
		_ = os.RemoveAll(filepath.Join(dir, "plugin-data"))
	}

	var report Report
	for _, file := range testFiles {
		results, err := plugin.RunTestFile(src, man, deps, file, plugin.TestOptions{Filter: opts.Filter, Reset: reset})
		if err != nil {
			report.Errors = append(report.Errors, fmt.Errorf("%s: %w", file, err))
			continue
		}
		report.Results = append(report.Results, results...)
	}
	return report, nil
}

// Print writes a report the way go test does.
func Print(w io.Writer, name string, r Report, verbose bool) {
	failed := 0
	for _, t := range r.Results {
		switch {
		case !t.OK:
			failed++
			fmt.Fprintf(w, "--- FAIL: %s › %s (%s)\n", t.File, t.Name, t.Time.Round(time.Millisecond))
			for _, line := range strings.Split(t.Error, "\n") {
				fmt.Fprintf(w, "    %s\n", line)
			}
		case verbose:
			fmt.Fprintf(w, "--- PASS: %s › %s (%s)\n", t.File, t.Name, t.Time.Round(time.Millisecond))
		}
	}
	for _, err := range r.Errors {
		fmt.Fprintf(w, "--- ERROR: %v\n", err)
	}
	status := "ok"
	if !r.Passed() {
		status = "FAIL"
	}
	fmt.Fprintf(w, "%s\t%s\t%d tests, %d failed\n", status, name, len(r.Results), failed+len(r.Errors))
}

// ErrFailed is returned by callers that turn a failing report into an error.
var ErrFailed = errors.New("plugin tests failed")
