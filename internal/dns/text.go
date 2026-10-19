package dns

import (
	"bufio"
	"net/netip"
	"strings"
)

// A lookup is recorded, held and edited as text shaped like an HTTP
// message, so the message view, the editor and interception treat it like
// any other: a start line, "Name: value" lines and a blank line.
//
//	QUERY app.example.test DNS
//	Type: A, AAAA
//	Resolver: this machine
//
//	DNS NOERROR
//	A: 10.0.0.12
//	AAAA: fd00::12
//
// Editing the query's name looks up the new name instead; editing the
// answer's A and AAAA lines changes the addresses connected to.

// Status is the outcome of a lookup, named as DNS names its response codes
// (plus UPSTREAM, for a name passed on for the upstream proxy to resolve).
type Status string

const (
	StatusNoError  Status = "NOERROR"
	StatusNXDomain Status = "NXDOMAIN"
	StatusServFail Status = "SERVFAIL"
	StatusTimeout  Status = "TIMEOUT"
	StatusUpstream Status = "UPSTREAM"
)

const (
	resolverLocal    = "this machine"
	resolverUpstream = "the upstream proxy (the name is passed on; add A or AAAA lines to connect to an address instead)"
)

// QueryText is the request side of a lookup of name.
func QueryText(name string, remote bool) []byte {
	resolver := resolverLocal
	if remote {
		resolver = resolverUpstream
	}
	return []byte("QUERY " + name + " DNS\r\nType: A, AAAA\r\nResolver: " + resolver + "\r\n\r\n")
}

// AnswerText is the response side of a lookup: its status and addresses.
func AnswerText(status Status, addrs []netip.Addr) []byte {
	var b strings.Builder
	b.WriteString("DNS " + string(status) + "\r\n")
	for _, a := range addrs {
		if a.Is4() || a.Is4In6() {
			b.WriteString("A: " + a.Unmap().String() + "\r\n")
		} else {
			b.WriteString("AAAA: " + a.String() + "\r\n")
		}
	}
	b.WriteString("\r\n")
	return []byte(b.String())
}

// ParseQuery reads the name from a query, as edited. ok is false when the
// start line no longer names one.
func ParseQuery(raw []byte) (name string, ok bool) {
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	if !sc.Scan() {
		return "", false
	}
	f := strings.Fields(sc.Text())
	if len(f) < 2 || !strings.EqualFold(f[0], "QUERY") {
		return "", false
	}
	name = normalize(f[1])
	return name, name != ""
}

// ParseAnswer reads the addresses from an answer, as edited: every A and
// AAAA line that holds an address, in order. A line that doesn't parse is
// skipped rather than failing the lookup.
func ParseAnswer(raw []byte) []netip.Addr {
	var out []netip.Addr
	sc := bufio.NewScanner(strings.NewReader(string(raw)))
	first := true
	for sc.Scan() {
		if first {
			first = false
			continue // the status line
		}
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		switch strings.ToUpper(strings.TrimSpace(key)) {
		case "A", "AAAA":
			if a, err := netip.ParseAddr(strings.TrimSpace(value)); err == nil {
				out = append(out, a.Unmap())
			}
		}
	}
	return out
}

// Summary is the one line a lookup shows in the request list: its
// addresses, or why it has none.
func Summary(status Status, addrs []netip.Addr) string {
	if len(addrs) == 0 {
		if status == StatusUpstream {
			return "resolved by the upstream proxy"
		}
		return "no addresses"
	}
	parts := make([]string, len(addrs))
	for i, a := range addrs {
		parts[i] = a.String()
	}
	return strings.Join(parts, ", ")
}

// normalize lower-cases a name and drops a trailing root dot.
func normalize(name string) string {
	return strings.TrimSuffix(strings.ToLower(strings.TrimSpace(name)), ".")
}
