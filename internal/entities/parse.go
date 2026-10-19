package entities

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"reproxy/internal/bodyview"
	"reproxy/internal/capture"
)

// Limits on the work done per flow.
const (
	maxBody       = 2 << 20 // bytes of a body read for fields or links
	maxFields     = 200     // fields kept from one request body
	maxLinksFound = 500     // links kept from one response
	maxDepth      = 4       // JSON nesting flattened into dotted names
	// maxCompression is how far a compressed body is assumed to expand: it
	// is read up to this multiple of maxBody, so that a compressed page
	// still yields maxBody bytes to scan.
	maxCompression = 20
)

// Parse works out what a flow contributes to the Explorer: the visit it
// made, the URLs its response refers to (on any host: a target's page
// pointing at a CDN is how related hosts are found), and, when the request
// came from a page on another host, that referral. ok is false
// when the flow isn't a request for a path (CONNECT, for instance).
func Parse(f capture.Flow) (v Visit, links []Discovery, ok bool) {
	if f.Method == "" || f.Method == http.MethodConnect {
		return Visit{}, nil, false
	}
	u, err := url.ParseRequestURI(f.Path)
	if err != nil {
		return Visit{}, nil, false
	}
	scheme := "http"
	if f.Secure {
		scheme = "https"
	}
	host := f.Host
	if u.Host != "" { // absolute-form, as sent to a plain HTTP proxy
		host = u.Host
	}
	if host == "" {
		return Visit{}, nil, false
	}
	addr := AddressOf(scheme, host, u.Path)
	v = Visit{
		FlowID:     f.ID,
		TargetID:   f.TargetID,
		Address:    addr,
		Method:     f.Method,
		URL:        addr.Scheme + "://" + addr.Host + addr.Concrete,
		Seen:       f.Started,
		Status:     f.StatusCode,
		StatusText: f.StatusText,
	}
	v.Query = valuesFields(u.Query(), queryOrder(u.RawQuery))

	if req, err := http.ReadRequest(bufio.NewReader(bytes.NewReader(f.Request.Raw))); err == nil {
		body := readBody(req.Body, req.Header)
		v.BodyType, v.Body = bodyFields(req.Header.Get("Content-Type"), body)
		if d, ok := referral(req.Header, addr, f); ok {
			links = append(links, d)
		}
	}

	if len(f.Response.Raw) > 0 {
		req := &http.Request{Method: f.Method}
		if resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(f.Response.Raw)), req); err == nil {
			v.RespType = mediaType(resp.Header.Get("Content-Type"))
			v.Server = strings.TrimSpace(resp.Header.Get("Server"))
			readAs := specType(v.RespType, u.Path)
			var body []byte
			if linkable(readAs) {
				body = readBody(resp.Body, resp.Header)
			} else {
				resp.Body.Close()
			}
			base := &url.URL{Scheme: scheme, Host: addr.Host, Path: u.Path}
			for _, l := range FindLinks(readAs, body, resp.Header, base) {
				links = append(links, Discovery{
					TargetID: f.TargetID,
					Address:  AddressOf(l.URL.Scheme, l.URL.Host, l.URL.Path),
					Link: Link{Source: OriginLink, FromURL: v.URL, FromFlowID: f.ID,
						Detail: l.Detail, Added: f.Started},
				})
			}
		}
	}
	v.Kind = KindOf(v.RespType, u.Path)
	return v, links, true
}

// referral is what a request's Referer (or, without one, Origin) says
// about the URL it was made to: that a page on another host led
// here, as a page on a target loads a script from a CDN. A referrer on the
// same host says nothing the store doesn't already know.
func referral(h http.Header, addr Address, f capture.Flow) (Discovery, bool) {
	raw, detail := h.Get("Referer"), "referer"
	if raw == "" {
		raw, detail = h.Get("Origin"), "origin"
	}
	ref, err := url.Parse(raw)
	if raw == "" || err != nil || ref.Host == "" || (ref.Scheme != "http" && ref.Scheme != "https") {
		return Discovery{}, false
	}
	from := AddressOf(ref.Scheme, ref.Host, ref.Path)
	if from.Host == addr.Host {
		return Discovery{}, false
	}
	at := addr
	at.Values = nil // the visit has counted them already
	return Discovery{
		TargetID: f.TargetID,
		Address:  at,
		Link: Link{Source: OriginReferer, FromURL: from.Scheme + "://" + from.Host + from.Concrete,
			FromFlowID: f.ID, Detail: detail, Added: f.Started},
	}, true
}

// splitPath splits a path into its non-empty segments, so "/a//b/" is a/b.
func splitPath(path string) []string {
	var segs []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			segs = append(segs, s)
		}
	}
	return segs
}

// queryOrder lists query names in the order they first appear, which
// url.Values (a map) loses.
func queryOrder(raw string) []string {
	var names []string
	seen := map[string]bool{}
	for _, kv := range strings.Split(raw, "&") {
		name, _, _ := strings.Cut(kv, "=")
		if n, err := url.QueryUnescape(name); err == nil && n != "" && !seen[n] {
			seen[n] = true
			names = append(names, n)
		}
	}
	return names
}

func valuesFields(v url.Values, order []string) []Field {
	var out []Field
	for _, name := range order {
		for _, val := range v[name] {
			out = append(out, Field{Name: name, Type: scalarType(val), Value: val})
		}
	}
	return out
}

// readBody reads a message body and undoes its content coding, giving back up
// to maxBody bytes of it. A truncated capture gives what was read.
func readBody(body io.ReadCloser, h http.Header) []byte {
	defer body.Close()
	// http.ReadResponse has already undone any chunked framing, so only the
	// content coding is left.
	coding := h.Get("Content-Encoding")
	limit := int64(maxBody)
	if coding != "" {
		limit *= maxCompression // maxBody caps what is scanned, not what arrives
	}
	raw, _ := io.ReadAll(io.LimitReader(body, limit))
	b := bodyview.Decode(raw, bodyview.Headers{ContentEncoding: coding}, true).Decoded
	if len(b) > maxBody {
		b = b[:maxBody]
	}
	return b
}

func mediaType(contentType string) string {
	if contentType == "" {
		return ""
	}
	mt, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return strings.ToLower(strings.TrimSpace(strings.SplitN(contentType, ";", 2)[0]))
	}
	return mt
}

// bodyFields returns a request body's media type and its fields, for the
// body formats whose fields can be named.
func bodyFields(contentType string, body []byte) (string, []Field) {
	if len(body) == 0 {
		return "", nil
	}
	mt := mediaType(contentType)
	switch {
	case mt == "application/x-www-form-urlencoded":
		v, err := url.ParseQuery(string(body))
		if err != nil {
			return mt, nil
		}
		return mt, valuesFields(v, queryOrder(string(body)))
	case mt == "multipart/form-data":
		return mt, multipartFields(contentType, body)
	case mt == "application/json" || strings.HasSuffix(mt, "+json"):
		var v any
		if err := json.Unmarshal(body, &v); err != nil {
			return mt, nil
		}
		var out []Field
		flattenJSON("", v, 0, &out)
		return mt, out
	}
	return mt, nil
}

func multipartFields(contentType string, body []byte) []Field {
	_, params, err := mime.ParseMediaType(contentType)
	if err != nil || params["boundary"] == "" {
		return nil
	}
	r := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	var out []Field
	for len(out) < maxFields {
		part, err := r.NextPart()
		if err != nil {
			break
		}
		name := part.FormName()
		if name == "" {
			continue
		}
		if part.FileName() != "" {
			out = append(out, Field{Name: name, Type: "file", Value: part.FileName()})
			continue
		}
		val, _ := io.ReadAll(io.LimitReader(part, 200))
		out = append(out, Field{Name: name, Type: scalarType(string(val)), Value: string(val)})
	}
	return out
}

// flattenJSON turns a JSON value into named fields: object members join
// with dots, array elements are named after the array with [] (only the
// first element is described, as a sample of the rest).
func flattenJSON(name string, v any, depth int, out *[]Field) {
	if len(*out) >= maxFields {
		return
	}
	switch t := v.(type) {
	case map[string]any:
		if name != "" {
			*out = append(*out, Field{Name: name, Type: "object"})
		}
		if depth >= maxDepth {
			return
		}
		for _, k := range sortedKeys(t) {
			child := k
			if name != "" {
				child = name + "." + k
			}
			flattenJSON(child, t[k], depth+1, out)
		}
	case []any:
		if name == "" {
			name = "[]"
		} else {
			*out = append(*out, Field{Name: name, Type: "array"})
			name += "[]"
		}
		if len(t) > 0 && depth < maxDepth {
			flattenJSON(name, t[0], depth+1, out)
		}
	case string:
		*out = append(*out, Field{Name: name, Type: "string", Value: t})
	case float64:
		typ := "number"
		if t == float64(int64(t)) {
			typ = "integer"
		}
		*out = append(*out, Field{Name: name, Type: typ, Value: strconv.FormatFloat(t, 'f', -1, 64)})
	case bool:
		*out = append(*out, Field{Name: name, Type: "boolean", Value: strconv.FormatBool(t)})
	case nil:
		*out = append(*out, Field{Name: name, Type: "null"})
	}
}

// sortedKeys returns m's keys in a stable order (JSON object order isn't
// kept by encoding/json).
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// scalarType infers the type of a text value: integer, number, boolean or
// string.
func scalarType(v string) string {
	if v == "" {
		return "string"
	}
	if _, err := strconv.ParseInt(v, 10, 64); err == nil {
		return "integer"
	}
	if _, err := strconv.ParseFloat(v, 64); err == nil && !strings.ContainsAny(v, "xXnN") { // not hex, Inf or NaN
		return "number"
	}
	if v == "true" || v == "false" {
		return "boolean"
	}
	return "string"
}

// ---- Identifiers ---------------------------------------------------------

var (
	uuidRe  = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	hexRe   = regexp.MustCompile(`^[0-9a-fA-F]{16,}$`)
	tokenRe = regexp.MustCompile(`^[A-Za-z0-9_\-]{20,}$`)
)

// isIdentifier reports whether a path segment looks like a record's
// identifier rather than a fixed name: a number, a UUID, a long hex string
// (a hash or object ID) or a long token mixing letters and digits.
func isIdentifier(seg string) bool {
	if seg == "" {
		return false
	}
	if allDigits(seg) || uuidRe.MatchString(seg) {
		return true
	}
	if hexRe.MatchString(seg) && hasDigit(seg) {
		return true
	}
	return tokenRe.MatchString(seg) && hasDigit(seg) && strings.IndexFunc(seg, isLetter) >= 0
}

func allDigits(s string) bool {
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func hasDigit(s string) bool { return strings.ContainsAny(s, "0123456789") }

func isLetter(c rune) bool { return c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' }

// ---- Links ---------------------------------------------------------------

// ExtractURLs returns the http(s) URLs a response body of media type mt
// refers to, resolved against base, each once (see FindURLs, which also
// says where each was found). limit caps how many are returned (0:
// maxLinksFound).
func ExtractURLs(mt string, body []byte, base *url.URL, limit int) []*url.URL {
	found := FindURLs(mt, body, nil, base, limit)
	out := make([]*url.URL, len(found))
	for i, f := range found {
		out[i] = f.URL
	}
	return out
}

// ResponseURLs returns every http(s) URL a raw HTTP response refers to (see
// FindURLs), with its body's chunked framing and content coding undone
// first. method is the request's (a HEAD response has no body); base is the
// URL requested. It is what Autocrawl follows.
func ResponseURLs(raw []byte, method string, base *url.URL) []Found {
	resp, err := http.ReadResponse(bufio.NewReader(bytes.NewReader(raw)), &http.Request{Method: method})
	if err != nil {
		return nil
	}
	mt := specType(mediaType(resp.Header.Get("Content-Type")), base.Path)
	var body []byte
	if linkable(mt) {
		body = readBody(resp.Body, resp.Header)
	} else {
		resp.Body.Close()
	}
	return FindURLs(mt, body, resp.Header, base, 0)
}

// FindLinks returns the URLs, on any host, that a response of media
// type mt refers to in its headers or body. base is the URL the response
// came from; relative links resolve against it.
func FindLinks(mt string, body []byte, header http.Header, base *url.URL) []Found {
	var out []Found
	seen := map[string]bool{}
	for _, f := range FindURLs(mt, body, header, base, 0) {
		a := AddressOf(f.URL.Scheme, f.URL.Host, f.URL.Path)
		key := a.Scheme + "://" + a.Host + a.Concrete
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, f)
	}
	return out
}
