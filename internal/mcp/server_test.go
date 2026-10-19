package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"reproxy/internal/config"
)

// serve mounts the registry over httptest and returns a connect helper that
// speaks the given protocol version with the given bearer token.
func (f *fixture) serve(t *testing.T, policy string) *httptest.Server {
	t.Helper()
	h := Handler(f.reg, Options{
		Token:  func() (string, error) { return "sekret", nil },
		Policy: func() string { return policy },
	})
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	return srv
}

func connect(t *testing.T, srv *httptest.Server, token, version string) *sdk.ClientSession {
	t.Helper()
	client := sdk.NewClient(&sdk.Implementation{Name: "test-client", Version: "1.0"}, nil)
	transport := &sdk.StreamableClientTransport{
		Endpoint:   srv.URL + Path,
		HTTPClient: &http.Client{Transport: bearer{token}},
	}
	cs, err := client.Connect(context.Background(), transport, &sdk.ClientSessionOptions{ProtocolVersion: version})
	if err != nil {
		t.Fatalf("connect (%s): %v", version, err)
	}
	t.Cleanup(func() { cs.Close() })
	return cs
}

// bearer adds the Authorization header to every request.
type bearer struct{ token string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	if b.token != "" {
		r.Header.Set("Authorization", "Bearer "+b.token)
	}
	return http.DefaultTransport.RoundTrip(r)
}

func callTool(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	return res
}

func resultText(res *sdk.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*sdk.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}

func TestServerProtocolVersionsAndTools(t *testing.T) {
	f := newFixture(t)
	f.addFlow("GET", "a.test", "/one", "")
	srv := f.serve(t, config.PolicyAuto)

	for _, version := range []string{"2026-07-28", "2025-11-25"} {
		t.Run(version, func(t *testing.T) {
			cs := connect(t, srv, "sekret", version)

			tools, err := cs.ListTools(context.Background(), nil)
			if err != nil {
				t.Fatal(err)
			}
			if len(tools.Tools) != len(f.reg.Tools()) {
				t.Fatalf("listed %d tools, registry has %d", len(tools.Tools), len(f.reg.Tools()))
			}

			res := callTool(t, cs, "list_flows", nil)
			if res.IsError || !strings.Contains(resultText(res), "abcd1234.f1") {
				t.Fatalf("list_flows over MCP = %q (isError=%v)", resultText(res), res.IsError)
			}
		})
	}
}

func TestServerRejectsMissingToken(t *testing.T) {
	f := newFixture(t)
	srv := f.serve(t, config.PolicyAuto)

	// A raw POST without the token is 401, before any MCP handshake.
	req, _ := http.NewRequest(http.MethodPost, srv.URL+Path, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"initialize"}`))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("no token: status %d, want 401", resp.StatusCode)
	}

	// A wrong token fails the handshake too.
	client := sdk.NewClient(&sdk.Implementation{Name: "t", Version: "1"}, nil)
	transport := &sdk.StreamableClientTransport{
		Endpoint:   srv.URL + Path,
		HTTPClient: &http.Client{Transport: bearer{"wrong"}},
	}
	if _, err := client.Connect(context.Background(), transport, nil); err == nil {
		t.Fatal("connect with wrong token succeeded")
	}
}

func TestServerScopeRefusalIsToolError(t *testing.T) {
	f := newFixture(t)
	id := f.addFlow("GET", "a.test", "/r", "")
	srv := f.serve(t, config.PolicyAuto)
	cs := connect(t, srv, "sekret", "2026-07-28")

	res := callTool(t, cs, "send_request", map[string]any{
		"flow":    flowHandle(testInstance, id),
		"request": "GET /r HTTP/1.1\nHost: evil.test\n\n",
	})
	// A scope refusal is reported as a tool error the model can read, not a
	// protocol-level failure.
	if !res.IsError || !strings.Contains(resultText(res), "scope") {
		t.Fatalf("scope refusal = %q (isError=%v)", resultText(res), res.IsError)
	}
}

func TestServerAskModeBlocksUntilDecided(t *testing.T) {
	f := newFixture(t)
	id := f.addFlow("GET", "a.test", "/r", "")
	srv := f.serve(t, config.PolicyAsk)
	cs := connect(t, srv, "sekret", "2026-07-28")

	done := make(chan *sdk.CallToolResult, 1)
	go func() {
		res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{
			Name:      "replay_create",
			Arguments: map[string]any{"flow": flowHandle(testInstance, id)},
		})
		if err != nil {
			t.Errorf("CallTool: %v", err)
		}
		done <- res
	}()

	select {
	case <-done:
		t.Fatal("replay_create returned before the user decided")
	case <-time.After(50 * time.Millisecond):
	}

	reqs := waitPending(t, f.broker, 1)
	if reqs[0].Client != "test-client" || reqs[0].Tool != "replay_create" {
		t.Fatalf("pending = %+v", reqs[0])
	}
	f.broker.Decide(reqs[0].ID, true)

	res := <-done
	if res.IsError || !strings.Contains(resultText(res), sessionHandle(testInstance, 1)) {
		t.Fatalf("approved replay_create = %q (isError=%v)", resultText(res), res.IsError)
	}
}
