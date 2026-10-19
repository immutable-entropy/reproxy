package plugin

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/dop251/goja"

	"reproxy/internal/capture"
	"reproxy/internal/replay"
)

// Limits on a plugin's outbound requests.
const (
	defaultSendTimeout = 30 * time.Second
	maxSendTimeout     = 5 * time.Minute
	// maxResponse is the most a response may be: the proxy's own capture
	// limit (proxy.DefaultCaptureLimit), as a plugin's response is held in
	// memory whole.
	maxResponse = 16 << 20
)

// outbound is a request ready to send.
type outbound struct {
	addr   string
	secure bool
	raw    []byte
	method string
	url    string
	host   string
	path   string
}

// send is http.send: a direct exchange through the upstream chain, or one
// through reproxy's own proxy so it is recorded.
func (r *Running) send(req map[string]any, opts map[string]any) *goja.Promise {
	out, err := buildRequest(req)
	throughProxy, _ := opts["throughProxy"].(bool)
	record, _ := opts["record"].(bool)
	timeout := defaultSendTimeout
	if ms, ok := opts["timeoutMs"].(int64); ok && ms > 0 {
		timeout = min(time.Duration(ms)*time.Millisecond, maxSendTimeout)
	}
	depth := r.depth + 1
	if err == nil && throughProxy && depth > MaxDepth {
		err = fmt.Errorf("E_RECURSION: %s would send through the proxy at depth %d (the limit is %d)", r.src.Name, depth, MaxDepth)
	}
	if err == nil && throughProxy && r.deps.ProxyDial == nil {
		err = errors.New("http.send: the proxy isn't running, so nothing can be sent through it")
	}
	// A plugin's traffic always leaves through the upstream chain: a nil
	// Dial would have replay connect directly, past the user's proxies,
	// so it is refused rather than allowed to fall back.
	if err == nil && !throughProxy && r.deps.Dial == nil {
		err = errors.New("http.send: no upstream dialer is set up, and a plugin may not connect directly")
	}

	return r.async(func() (func(*goja.Runtime) goja.Value, error) {
		if err != nil {
			return nil, err
		}
		select {
		case r.sem <- struct{}{}:
			defer func() { <-r.sem }()
		case <-r.ctx.Done():
			return nil, r.ctx.Err()
		}
		ctx, cancel := context.WithTimeout(r.ctx, timeout)
		defer cancel()
		ctx = capture.WithSource(ctx, capture.SourcePlugin, r.src.Name) // for the lookup, if one is made

		rq := replay.Request{Addr: out.addr, Secure: out.secure, Raw: out.raw, Dial: r.deps.Dial, Limit: maxResponse}
		if throughProxy {
			label := OriginLabel(r.src.Name, depth)
			rq.Dial = func(ctx context.Context, network, addr string) (net.Conn, error) {
				return r.deps.ProxyDial(ctx, network, addr, label)
			}
		}

		var flowID uint64
		started := time.Now()
		if record && !throughProxy {
			flowID = r.deps.Store.Add(capture.Flow{
				Target: out.addr, Method: out.method, Host: out.host, Path: out.path, Secure: out.secure,
				Source: capture.SourcePlugin, Origin: r.src.Name,
				Request: capture.Message{Raw: out.raw, Size: int64(len(out.raw))}, Started: started,
			})
		}
		resp, err := r.deps.Send(ctx, rq)
		if flowID != 0 {
			if err != nil {
				r.deps.Store.Fail(flowID, err.Error(), time.Now())
			} else {
				r.deps.Store.Complete(flowID, capture.Message{Raw: resp.Raw, Size: int64(resp.Size)},
					resp.StatusCode, resp.Status, started.Add(resp.Duration))
			}
		}
		if err != nil {
			return nil, fmt.Errorf("http.send %s %s: %w", out.method, out.url, err)
		}
		text := strings.TrimSpace(strings.TrimPrefix(resp.Status, strconv.Itoa(resp.StatusCode)))
		return func(vm *goja.Runtime) goja.Value {
			return responseValue(vm, resp.Raw, resp.StatusCode, text, resp.Duration, flowID, out.method)
		}, nil
	})
}

// buildRequest turns the SDK's Request (or RawRequest) into bytes for the
// wire: HTTP/1.1, one exchange, Connection: close.
func buildRequest(req map[string]any) (outbound, error) {
	raw, _ := req["url"].(string)
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return outbound{}, fmt.Errorf("http.send: %q is not an absolute http(s) URL", raw)
	}
	out := outbound{secure: u.Scheme == "https", url: u.String(), host: u.Host, path: u.RequestURI()}
	port := u.Port()
	if port == "" {
		port = map[bool]string{true: "443", false: "80"}[out.secure]
	}
	out.addr = net.JoinHostPort(u.Hostname(), port)

	if text, ok := req["raw"].(string); ok {
		text = strings.ReplaceAll(strings.ReplaceAll(text, "\r\n", "\n"), "\n", "\r\n")
		head, body, _ := strings.Cut(text, "\r\n\r\n")
		out.raw = []byte(head + "\r\n\r\n" + body)
		out.method, _, _ = strings.Cut(head, " ")
		return out, nil
	}

	out.method = "GET"
	if m, ok := req["method"].(string); ok && m != "" {
		out.method = strings.ToUpper(m)
	}
	var body []byte
	switch b := req["body"].(type) {
	case string:
		body = []byte(b)
	case []byte:
		body = b
	case goja.ArrayBuffer:
		body = b.Bytes()
	}
	headers := headerPairs(req["headers"])

	var buf bytes.Buffer
	fmt.Fprintf(&buf, "%s %s HTTP/1.1\r\n", out.method, out.path)
	has := func(name string) bool {
		for _, h := range headers {
			if strings.EqualFold(h[0], name) {
				return true
			}
		}
		return false
	}
	if !has("Host") {
		fmt.Fprintf(&buf, "Host: %s\r\n", u.Host)
	}
	for _, h := range headers {
		if strings.ContainsAny(h[0]+h[1], "\r\n") {
			return outbound{}, fmt.Errorf("http.send: header %q contains a line break", h[0])
		}
		fmt.Fprintf(&buf, "%s: %s\r\n", h[0], h[1])
	}
	if len(body) > 0 && !has("Content-Length") && !has("Transfer-Encoding") {
		fmt.Fprintf(&buf, "Content-Length: %d\r\n", len(body))
	}
	if !has("Connection") {
		buf.WriteString("Connection: close\r\n")
	}
	if !has("User-Agent") {
		buf.WriteString("User-Agent: reproxy-plugin\r\n")
	}
	buf.WriteString("\r\n")
	buf.Write(body)
	out.raw = buf.Bytes()
	return out, nil
}

// headerPairs reads headers given as an object or as [name, value] pairs.
func headerPairs(v any) [][2]string {
	var out [][2]string
	switch h := v.(type) {
	case map[string]any:
		names := make([]string, 0, len(h))
		for k := range h {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			out = append(out, [2]string{k, fmt.Sprint(h[k])})
		}
	case []any:
		for _, pair := range h {
			if p, ok := pair.([]any); ok && len(p) == 2 {
				out = append(out, [2]string{fmt.Sprint(p[0]), fmt.Sprint(p[1])})
			}
		}
	}
	return out
}
