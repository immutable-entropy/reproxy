package jsrt

import (
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"time"

	"github.com/dop251/goja"
)

var sdk = map[string]string{
	"reproxy": `export const answer: number = 42; export function hello(n: string) { return "hi " + n }`,
}

func TestBuildBundlesImportsAndTheSDK(t *testing.T) {
	fsys := fstest.MapFS{
		"main.ts":       {Data: []byte(`import { greet } from "./src/greet"; import data from "./data.json"; export function run() { return greet() + data.n }`)},
		"src/greet.ts":  {Data: []byte(`import { hello, answer } from "reproxy"; export const greet = (): string => hello("x") + answer`)},
		"data.json":     {Data: []byte(`{"n": 1}`)},
		"src/unused.ts": {Data: []byte(`export {}`)},
	}
	b, err := Build(fsys, "main.ts", sdk)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(b.Files, []string{"data.json", "main.ts", "src/greet.ts"}) {
		t.Errorf("files = %v", b.Files)
	}
	exports, err := Exports(fsys, "main.ts", sdk)
	if err != nil || !reflect.DeepEqual(exports, []string{"run"}) {
		t.Fatalf("exports = %v, %v", exports, err)
	}

	r := New(Options{})
	defer r.Close()
	var got string
	err = r.Do(0, func(r *Runtime) error {
		if _, err := r.VM().RunScript("plugin.js", b.JS); err != nil {
			return err
		}
		fn, _ := goja.AssertFunction(r.VM().Get(GlobalName).ToObject(r.VM()).Get("run"))
		v, err := r.Call(fn)
		if err == nil {
			got = v.String()
		}
		return err
	})
	if err != nil || got != "hi x421" {
		t.Fatalf("run = %q, %v", got, err)
	}
}

func TestSandboxRefusesImportsOutsideThePlugin(t *testing.T) {
	for name, src := range map[string]string{
		"parent":   `import x from "../secret"`,
		"builtin":  `import x from "fs"`,
		"node":     `import x from "node:fs"`,
		"package":  `import x from "lodash"`,
		"absolute": `import x from "/etc/passwd"`,
		"missing":  `import x from "./nope"`,
		"text":     `import x from "./notes.txt"`,
	} {
		fsys := fstest.MapFS{"main.ts": {Data: []byte(src + "; export const a = x")}, "notes.txt": {Data: []byte("x")}}
		if _, err := Build(fsys, "main.ts", sdk); err == nil {
			t.Errorf("%s: bundled", name)
		}
	}

	// A symlink out of the plugin's directory is no way out either, when the
	// plugin is read through an os.Root.
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "outside.ts"), []byte(`export const x = 1`), 0o644)
	plugin := filepath.Join(dir, "p")
	os.Mkdir(plugin, 0o755)
	os.WriteFile(filepath.Join(plugin, "main.ts"), []byte(`import { x } from "./link"; export const y = x`), 0o644)
	if err := os.Symlink(filepath.Join(dir, "outside.ts"), filepath.Join(plugin, "link.ts")); err != nil {
		t.Skip("no symlinks:", err)
	}
	root, err := os.OpenRoot(plugin)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	if _, err := Build(root.FS(), "main.ts", sdk); err == nil {
		t.Error("a symlink out of the plugin was followed")
	}
}

func TestBuildErrorsNameTheFileAndLine(t *testing.T) {
	_, err := Build(fstest.MapFS{"main.ts": {Data: []byte("export const a = ;")}}, "main.ts", sdk)
	var be *BuildError
	if !errors.As(err, &be) || !strings.Contains(err.Error(), "main.ts:1:") {
		t.Fatalf("err = %v", err)
	}
}

func TestBudgetInterruptsARunawayTurn(t *testing.T) {
	r := New(Options{Budget: 50 * time.Millisecond})
	defer r.Close()
	start := time.Now()
	err := r.Do(0, func(r *Runtime) error {
		_, err := r.VM().RunString(`for (;;) {}`)
		return err
	})
	if !errors.Is(err, ErrBudget) {
		t.Fatalf("err = %v", err)
	}
	if time.Since(start) > 2*time.Second {
		t.Error("took too long to interrupt")
	}
	// The runtime is usable afterwards.
	if err := r.Do(0, func(r *Runtime) error { _, err := r.VM().RunString(`1+1`); return err }); err != nil {
		t.Fatal(err)
	}
}

func TestSettleAwaitsHostPromises(t *testing.T) {
	r := New(Options{})
	defer r.Close()
	got := make(chan string, 1)
	r.Do(0, func(r *Runtime) error {
		r.VM().Set("later", func() *goja.Promise {
			p, resolve, _ := r.Promise()
			go func() { time.Sleep(10 * time.Millisecond); resolve("done") }()
			return p
		})
		v, err := r.VM().RunString(`(async () => { const a = await later(); await null; return a + "!" })()`)
		if err != nil {
			return err
		}
		r.Settle(v, func(v goja.Value, err error) {
			if err != nil {
				got <- err.Error()
				return
			}
			got <- v.String()
		})
		return nil
	})
	select {
	case s := <-got:
		if s != "done!" {
			t.Fatalf("settled with %q", s)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("never settled")
	}

	r.Do(0, func(r *Runtime) error {
		v, _ := r.VM().RunString(`(async () => { throw new Error("nope") })()`)
		r.Settle(v, func(_ goja.Value, err error) { got <- err.Error() })
		return nil
	})
	if s := <-got; !strings.Contains(s, "nope") {
		t.Errorf("rejection = %q", s)
	}
}

func TestTimersAndGlobals(t *testing.T) {
	var mu sync.Mutex
	var logs []string
	r := New(Options{Log: func(_ slog.Level, msg string) { mu.Lock(); logs = append(logs, msg); mu.Unlock() }})
	defer r.Close()
	err := r.Do(0, func(r *Runtime) error {
		_, err := r.VM().RunString(`
			let n = 0;
			const id = setInterval(() => { if (++n === 3) { clearInterval(id); console.log("ticks", n); } }, 1);
			setTimeout((a, b) => console.log("timeout", a + b), 5, 2, 3);
			const cancelled = setTimeout(() => console.log("never"), 1); clearTimeout(cancelled);
			console.log(new TextDecoder().decode(new TextEncoder().encode("héllo")), atob(btoa("hi")), {a: 1});
			queueMicrotask(() => console.log("micro"));
		`)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(logs)
		mu.Unlock()
		if n >= 4 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	joined := strings.Join(logs, "|")
	for _, want := range []string{`héllo hi {"a":1}`, "micro", "ticks 3", "timeout 5"} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in %q", want, joined)
		}
	}
	if strings.Contains(joined, "never") {
		t.Error("a cleared timeout fired")
	}
}

func TestEventsWaitForRoom(t *testing.T) {
	events := make(chan func(*Runtime), 100)
	r := New(Options{Events: events})
	defer r.Close()
	var mu sync.Mutex
	ran := 0
	for i := 0; i < 20; i++ {
		events <- func(r *Runtime) {
			mu.Lock()
			ran++
			mu.Unlock()
			// Each handler awaits something that never settles.
			v, _ := r.VM().RunString(`new Promise(() => {})`)
			r.Settle(v, func(goja.Value, error) {})
		}
	}
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	if ran != maxInflight {
		t.Errorf("ran %d events with that many in flight, want %d", ran, maxInflight)
	}
}

func TestCloseStopsWork(t *testing.T) {
	r := New(Options{})
	go func() { time.Sleep(20 * time.Millisecond); r.Close() }()
	err := r.Do(10*time.Second, func(r *Runtime) error { _, err := r.VM().RunString(`for(;;){}`); return err })
	if !errors.Is(err, ErrClosed) {
		t.Errorf("err = %v", err)
	}
	if err := r.Do(0, func(*Runtime) error { return nil }); !errors.Is(err, ErrClosed) {
		t.Errorf("after close: %v", err)
	}
}
