// Command depsoak refuses Go module versions that haven't been public long
// enough to have been looked at by anyone else: every module go.mod
// requires (direct or indirect) and the Go toolchain it builds with must
// have been first published at least -days ago, unless deps-soak.allow
// names that exact version with a reason.
//
// A version's age can't be taken from the module proxy's Time: that is the
// commit's date, which whoever pushed the commit chose, so a backdated
// malicious release would pass. The checksum database (sum.golang.org) is an
// append-only log, and a version's record number there says when the Go
// ecosystem first saw it. depsoak finds the record number at the cutoff
// (the module index, index.golang.org, lists what the proxy first fetched at
// a given time; their records mark the log's size then) and refuses any
// version whose record is newer.
//
// It needs the network and fails closed: a version it can't place is
// refused.
//
//	go run ./tools/depsoak            # check go.mod in this directory
//	go run ./tools/depsoak -days 21
package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

var (
	days      = flag.Int("days", 14, "minimum age, in days, of every module version and the toolchain")
	allowFile = flag.String("allow", "deps-soak.allow", "exceptions: lines of `module version reason`")
	sumdb     = flag.String("sumdb", "https://sum.golang.org", "checksum database")
	index     = flag.String("index", "https://index.golang.org", "module index")
	proxy     = flag.String("proxy", "https://proxy.golang.org", "module proxy (only for the commit dates shown)")
)

var client = &http.Client{Timeout: 30 * time.Second}

type modVer struct{ Path, Version string }

func (m modVer) String() string { return m.Path + "@" + m.Version }

func main() {
	flag.Parse()
	code, err := run(context.Background(), os.Stdout)
	if err != nil {
		fmt.Fprintln(os.Stderr, "depsoak:", err)
		os.Exit(2)
	}
	os.Exit(code)
}

func run(ctx context.Context, out io.Writer) (int, error) {
	mods, err := required()
	if err != nil {
		return 0, err
	}
	allow, err := readAllow(*allowFile)
	if err != nil {
		return 0, err
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -*days)
	marker, err := cutoffRecord(ctx, cutoff)
	if err != nil {
		return 0, fmt.Errorf("finding the checksum database's size at %s: %w", cutoff.Format(time.RFC3339), err)
	}

	records := make([]int64, len(mods))
	errs := make([]error, len(mods))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i, m := range mods {
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			records[i], errs[i] = lookup(ctx, m)
		}()
	}
	wg.Wait()

	fmt.Fprintf(out, "Soak period %d days: versions first published after %s (checksum record %d) are refused.\n\n",
		*days, cutoff.Format("2006-01-02 15:04 MST"), marker)
	var young, allowed []modVer
	for i, m := range mods {
		switch {
		case errs[i] != nil:
			fmt.Fprintf(out, "REFUSED  %s: %v\n", m, errs[i])
			young = append(young, m)
		case records[i] <= marker:
			if _, ok := allow[m]; ok {
				fmt.Fprintf(out, "STALE    %s has soaked; remove its line from %s\n", m, *allowFile)
			}
		default:
			if reason, ok := allow[m]; ok {
				fmt.Fprintf(out, "ALLOWED  %s (%s)\n", m, reason)
				allowed = append(allowed, m)
				continue
			}
			note := ""
			if t, err := commitTime(ctx, m); err == nil && t.Before(cutoff) {
				note = fmt.Sprintf(" (its commit is dated %s, before the window: check it wasn't backdated)", t.Format("2006-01-02"))
			}
			fmt.Fprintf(out, "REFUSED  %s was first published within the last %d days%s\n", m, *days, note)
			young = append(young, m)
		}
	}
	for m := range allow {
		if !slices.Contains(mods, m) {
			fmt.Fprintf(out, "STALE    %s is in %s but not required; remove its line\n", m, *allowFile)
		}
	}
	fmt.Fprintf(out, "\n%d versions checked, %d allowed by exception, %d refused.\n", len(mods), len(allowed), len(young))
	if len(young) > 0 {
		fmt.Fprintf(out, "Pin an older version, or review the change and add `module version reason` to %s.\n", *allowFile)
		return 1, nil
	}
	return 0, nil
}

// required lists every module version go.mod requires, and the toolchain
// (as module "toolchain", version "go1.N.P").
func required() ([]modVer, error) {
	raw, err := exec.Command("go", "mod", "edit", "-json").Output()
	if err != nil {
		return nil, fmt.Errorf("reading go.mod: %w", err)
	}
	var gm struct {
		Go        string
		Toolchain string
		Require   []modVer
	}
	if err := json.Unmarshal(raw, &gm); err != nil {
		return nil, fmt.Errorf("reading go.mod: %w", err)
	}
	mods := gm.Require
	tc := gm.Toolchain
	if tc == "" && strings.Count(gm.Go, ".") == 2 {
		tc = "go" + gm.Go
	}
	if tc == "" {
		return nil, errors.New("go.mod names no exact toolchain: set `go 1.N.P` or a toolchain line")
	}
	mods = append(mods, modVer{"toolchain", tc})
	return mods, nil
}

// module is the module the checksum database and proxy know the version
// as: the toolchain is the golang.org/toolchain build for this platform.
func (m modVer) module() modVer {
	if m.Path == "toolchain" {
		return modVer{"golang.org/toolchain", "v0.0.1-" + m.Version + "." + runtime.GOOS + "-" + runtime.GOARCH}
	}
	return m
}

// readAllow reads the exceptions file: `module version reason`, # comments.
func readAllow(name string) (map[modVer]string, error) {
	allow := map[modVer]string{}
	f, err := os.Open(name)
	if errors.Is(err, os.ErrNotExist) {
		return allow, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for n := 1; sc.Scan(); n++ {
		line, _, _ := strings.Cut(sc.Text(), "#")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		if len(fields) < 3 {
			return nil, fmt.Errorf("%s:%d: want `module version reason`", name, n)
		}
		allow[modVer{fields[0], fields[1]}] = strings.Join(fields[2:], " ")
	}
	return allow, sc.Err()
}

// cutoffRecord returns the checksum database's record number at the cutoff:
// the lowest record among the first versions the proxy fetched from then.
// Taking the lowest errs toward refusing.
func cutoffRecord(ctx context.Context, cutoff time.Time) (int64, error) {
	body, err := get(ctx, *index+"/index?limit=20&since="+url.QueryEscape(cutoff.Format(time.RFC3339)))
	if err != nil {
		return 0, err
	}
	var marker int64 = -1
	found := 0
	dec := json.NewDecoder(strings.NewReader(string(body)))
	for found < 3 {
		var e struct{ Path, Version string }
		if err := dec.Decode(&e); err == io.EOF {
			break
		} else if err != nil {
			return 0, err
		}
		rec, err := lookup(ctx, modVer(e))
		if err != nil {
			continue // e.g. retracted from the database since
		}
		if marker < 0 || rec < marker {
			marker = rec
		}
		found++
	}
	if marker < 0 {
		return 0, errors.New("no module index entries could be placed in the checksum database")
	}
	return marker, nil
}

// lookup returns the version's record number in the checksum database.
func lookup(ctx context.Context, m modVer) (int64, error) {
	m = m.module()
	body, err := get(ctx, *sumdb+"/lookup/"+escape(m.Path)+"@"+escape(m.Version))
	if err != nil {
		return 0, err
	}
	first, _, _ := strings.Cut(string(body), "\n")
	n, err := strconv.ParseInt(strings.TrimSpace(first), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("unexpected checksum database answer %q", first)
	}
	return n, nil
}

// commitTime is the proxy's Time for the version: the commit's own date.
func commitTime(ctx context.Context, m modVer) (time.Time, error) {
	m = m.module()
	body, err := get(ctx, *proxy+"/"+escape(m.Path)+"/@v/"+escape(m.Version)+".info")
	if err != nil {
		return time.Time{}, err
	}
	var info struct{ Time time.Time }
	err = json.Unmarshal(body, &info)
	return info.Time, err
}

func get(ctx context.Context, u string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: %s", u, resp.Status)
	}
	return body, nil
}

// escape is the module proxy's case encoding: each capital letter becomes
// '!' and its lower case.
func escape(s string) string {
	var b strings.Builder
	for _, r := range s {
		if 'A' <= r && r <= 'Z' {
			b.WriteByte('!')
			r += 'a' - 'A'
		}
		b.WriteRune(r)
	}
	return b.String()
}
