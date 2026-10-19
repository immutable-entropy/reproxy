package app

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"reproxy/internal/capture"
	"reproxy/internal/crawl"
	"reproxy/internal/entities"
	"reproxy/internal/target"
	"reproxy/internal/upstream"
)

// connectProxy is an HTTP CONNECT proxy that counts the tunnels asked of
// it.
func connectProxy(t *testing.T) (port int, tunnels *atomic.Int32) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	tunnels = &atomic.Int32{}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				req, err := http.ReadRequest(bufio.NewReader(c))
				if err != nil || req.Method != http.MethodConnect {
					return
				}
				tunnels.Add(1)
				up, err := net.Dial("tcp", req.Host)
				if err != nil {
					fmt.Fprint(c, "HTTP/1.1 502 Bad Gateway\r\n\r\n")
					return
				}
				defer up.Close()
				fmt.Fprint(c, "HTTP/1.1 200 Connection established\r\n\r\n")
				go io.Copy(up, c)
				io.Copy(c, up)
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port, tunnels
}

// Every request Autocrawl makes goes out through the upstream chain, and
// is recorded as the crawl's.
func TestCrawlGoesThroughTheUpstreamChain(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/" {
			fmt.Fprint(w, `<a href="/next">next</a>`)
		}
	}))
	defer site.Close()

	a, _ := newApp(t, freePort(t))
	proxyPort, tunnels := connectProxy(t)
	cfg := a.Settings()
	cfg.Upstream = upstream.Chain{Enabled: true, Proxies: []upstream.Proxy{
		{ID: "u1", Enabled: true, Kind: upstream.HTTP, Host: "127.0.0.1", Port: proxyPort}}}
	if err := a.ApplySettings(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Targets().Create(target.KindIP, "127.0.0.1"); err != nil {
		t.Fatal(err)
	}

	// The Explorer knows the site (as if it had been browsed), so the
	// crawl starts from its origin.
	u, _ := url.Parse(site.URL)
	if _, _, err := a.Entities().Discover(entities.Discovery{
		Address: entities.AddressOf("http", u.Host, "/"),
		Link:    entities.Link{Source: entities.OriginLink},
	}); err != nil {
		t.Fatal(err)
	}

	c := a.Crawler()
	if err := c.Start(crawl.Settings{RequestsPerSecond: crawl.MaxRequestsPerSecond, Threads: 1, OnlyNew: true}); err != nil {
		t.Fatal(err)
	}
	c.Wait()

	crawled := 0
	for _, f := range a.Store().List() {
		if f.Source == capture.SourceCrawl && f.Protocol != capture.ProtocolDNS {
			crawled++
		}
	}
	if st, _ := c.State(site.URL + "/next"); st != crawl.Crawled {
		t.Errorf("the linked page is %v, want crawled", st)
	}
	if n := int(tunnels.Load()); crawled == 0 || n != crawled {
		t.Errorf("%d crawl requests, %d through the upstream proxy: every one must go through it", crawled, n)
	}
}
