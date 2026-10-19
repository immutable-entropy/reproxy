package proxy

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"reproxy/internal/capture"
	"reproxy/internal/upstream"
)

// A plugin's connection authenticates with credentials the origin resolver
// recognises, and its flows are labelled with the plugin; the browser's
// (no authentication) and a stranger's credentials are the proxy's.
func TestOriginLabelsPluginTraffic(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "ok") }))
	defer app.Close()

	store := capture.NewStore()
	srv := New(store, WithLogger(slog.New(slog.DiscardHandler)),
		WithOrigin(func(user, pass string) (capture.Source, string, bool) {
			if pass != "s3cret" {
				return "", "", false
			}
			return capture.SourcePlugin, user, true
		}))
	if err := srv.Listen("127.0.0.1:0"); err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	get := func(user, pass string) {
		t.Helper()
		d := upstream.NewDialer([]upstream.Hop{{Kind: upstream.SOCKS5, Addr: srv.Addr(), Username: user, Password: pass}})
		conn, err := d.DialContext(context.Background(), "tcp", app.Listener.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		io.WriteString(conn, "GET / HTTP/1.1\r\nHost: app\r\nConnection: close\r\n\r\n")
		resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	get("mapper", "s3cret")
	get("mapper", "wrong")
	get("", "")
	waitFor(t, "three flows", completed(store, 3))

	flows := store.List()
	if flows[0].Source != capture.SourcePlugin || flows[0].Origin != "mapper" {
		t.Errorf("plugin flow = %q/%q", flows[0].Source, flows[0].Origin)
	}
	for _, f := range flows[1:] {
		if f.Source != capture.SourceProxy || f.Origin != "" {
			t.Errorf("ordinary flow = %q/%q", f.Source, f.Origin)
		}
	}
}
