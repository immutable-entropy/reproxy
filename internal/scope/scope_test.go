package scope

import "testing"

func TestCheck(t *testing.T) {
	cases := []struct {
		origin, addr, host string
		ok                 bool
	}{
		{"api.example.com", "api.example.com:443", "api.example.com", true},
		{"api.example.com", "www.example.com:8443", "www.example.com", true},
		{"api.example.com:8080", "EXAMPLE.com.:80", "", true},
		{"api.example.com", "example.org:443", "", false},
		{"api.example.com", "api.example.com:443", "evil.test", false},
		{"a.example.co.uk", "b.example.co.uk:443", "", true},
		{"a.example.co.uk", "other.co.uk:443", "", false},
		// Hosts on a public-suffix-provided domain are separate sites.
		{"alice.github.io", "bob.github.io:443", "", false},
		{"localhost:3000", "localhost:4000", "localhost:4000", true},
		{"localhost", "otherhost:80", "", false},
		{"10.0.0.5:80", "10.0.0.5:8080", "", true},
		{"10.0.0.5", "10.0.0.6:80", "", false},
		{"[::1]:8080", "[::1]:9090", "", true},
		{"", "example.com:80", "", false},
	}
	for _, c := range cases {
		err := Check(c.origin, c.addr, c.host)
		if (err == nil) != c.ok {
			t.Errorf("Check(%q, %q, %q) = %v, want ok=%v", c.origin, c.addr, c.host, err, c.ok)
		}
	}
}
