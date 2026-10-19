package app

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"reproxy/internal/capture"
	"reproxy/internal/plugin"
	"reproxy/internal/upstream"
)

// A user plugin in the data directory is found at startup, and what it
// sends through the proxy is recorded as its traffic: through the real
// proxy, authenticated with the process's plugin secret.
func TestUserPluginTrafficThroughTheProxy(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "pong") }))
	defer site.Close()

	a, _ := newApp(t, freePort(t))
	dir := filepath.Join(plugin.UserDir(a.DataDir()), "pinger")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{
		"name": "pinger", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"targets": [{"hook": "plugin.on_load", "function": "init"}],
		"settings": [{"key": "url", "default": "`+site.URL+`/ping"}]
	}`), 0o644)
	os.WriteFile(filepath.Join(dir, "main.ts"), []byte(`
		import { http, settings, log } from "reproxy";
		export async function init() {
			const r = await http.send({ url: settings.get<string>("url") }, { throughProxy: true });
			log.info("got", r.status, r.text());
		}`), 0o644)

	a.Start()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var found *capture.Flow
		for _, f := range a.Store().List() {
			if f.Path == "/ping" && f.State == capture.StateComplete {
				found = &f
			}
		}
		if found != nil {
			if found.Source != capture.SourcePlugin || found.Origin != "pinger" {
				t.Fatalf("flow source/origin = %q/%q", found.Source, found.Origin)
			}
			break
		}
		if time.Now().After(deadline) {
			info, _ := a.Plugins().Get("pinger")
			t.Fatalf("no recorded flow; plugin %+v", info)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// A plugin target with hold: true rewrites a response on its way through
// the real proxy while interception is off, and the flow records the edit.
func TestPluginHoldRewritesThroughTheProxy(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"admin":false}`)
	}))
	defer site.Close()

	a, _ := newApp(t, freePort(t))
	dir := filepath.Join(plugin.UserDir(a.DataDir()), "flipper")
	os.MkdirAll(dir, 0o755)
	os.WriteFile(filepath.Join(dir, "plugin.json"), []byte(`{
		"name": "flipper", "version": "1.0", "author": {"name": "t"}, "status": "enabled",
		"targets": [{"hook": "intercept.on_response_pending", "function": "flip", "hold": true,
			"match": {"content_types": ["application/json"]}}]
	}`), 0o644)
	os.WriteFile(filepath.Join(dir, "main.ts"), []byte(`
		import { decision, edit, type PendingEvent } from "reproxy";
		export function flip(e: PendingEvent) {
			return decision.edit(edit.setBody(e.message.editable(), e.message.text().replace("false", "true")));
		}`), 0o644)
	a.Start()
	waitFor(t, "the plugin to start", func() bool {
		info, ok := a.Plugins().Get("flipper")
		return ok && info.State == plugin.StateRunning
	})

	addr, _ := a.ProxyStatus()
	conn, err := upstreamDial(addr, site.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	io.WriteString(conn, "GET /flag HTTP/1.1\r\nHost: site\r\n\r\n")
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.HasSuffix(string(body), `{"admin":true}`) {
		t.Fatalf("browser got %q", body)
	}
	waitFor(t, "the flow", func() bool {
		for _, f := range a.Store().List() {
			if f.Path == "/flag" && f.State == capture.StateComplete {
				return f.OriginalResponse != nil
			}
		}
		return false
	})
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// upstreamDial opens a tunnel to target through the proxy at addr, as a
// browser would.
func upstreamDial(addr, target string) (net.Conn, error) {
	return upstream.NewDialer([]upstream.Hop{{Kind: upstream.SOCKS5, Addr: addr}}).DialContext(context.Background(), "tcp", target)
}
