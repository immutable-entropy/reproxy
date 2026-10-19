package tlsca

import (
	"crypto/tls"
	"crypto/x509"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadOrCreatePersistsAndReloads(t *testing.T) {
	dir := t.TempDir()
	ca, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Key file must not be world-readable.
	info, err := os.Stat(filepath.Join(dir, "ca-key.pem"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("key perms = %o, want owner-only", perm)
	}

	again, err := LoadOrCreate(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(ca.CertPEM()) != string(again.CertPEM()) {
		t.Error("reload produced a different CA certificate")
	}
	if ca.Fingerprint() != again.Fingerprint() || len(ca.Fingerprint()) != 32*3-1 {
		t.Errorf("fingerprint mismatch or wrong length: %q", ca.Fingerprint())
	}
}

func TestLeafCertVerifiesAgainstCA(t *testing.T) {
	ca, err := LoadOrCreate(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(ca.CertPEM()) {
		t.Fatal("CA PEM not accepted into pool")
	}

	cases := []struct {
		host   string
		verify string
	}{
		{"stage.example.com", "stage.example.com"},
		{"stage.example.com:443", "stage.example.com"}, // port stripped
		{"10.0.0.5", "10.0.0.5"},
	}
	for _, c := range cases {
		leaf, err := ca.LeafFor(c.host)
		if err != nil {
			t.Fatalf("%s: %v", c.host, err)
		}
		if _, err := leaf.Leaf.Verify(x509.VerifyOptions{
			Roots:   roots,
			DNSName: dnsNameFor(c.verify),
		}); err != nil {
			t.Errorf("%s: leaf did not verify: %v", c.host, err)
		}
	}
}

// dnsNameFor returns a name suitable for VerifyOptions.DNSName; IP SANs are
// checked by passing the IP string too.
func dnsNameFor(host string) string { return host }

func TestLeafCacheReturnsSameCert(t *testing.T) {
	ca, _ := LoadOrCreate(t.TempDir())
	a, _ := ca.LeafFor("example.com")
	b, _ := ca.LeafFor("example.com:443")
	if a != b {
		t.Error("expected cached leaf to be reused across equal hosts")
	}
}

func TestLeafUsableInTLSHandshake(t *testing.T) {
	ca, _ := LoadOrCreate(t.TempDir())
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(ca.CertPEM())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	serverCfg := &tls.Config{GetCertificate: func(hi *tls.ClientHelloInfo) (*tls.Certificate, error) {
		return ca.LeafFor(hi.ServerName)
	}}
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		tlsConn := tls.Server(conn, serverCfg)
		tlsConn.Handshake()
		tlsConn.Write([]byte("ok"))
		tlsConn.Close()
	}()

	conn, err := tls.Dial("tcp", ln.Addr().String(), &tls.Config{RootCAs: roots, ServerName: "stage.local"})
	if err != nil {
		t.Fatalf("client handshake failed (CA not trusted?): %v", err)
	}
	defer conn.Close()
	buf := make([]byte, 2)
	conn.Read(buf)
	if string(buf) != "ok" {
		t.Errorf("read %q", buf)
	}
}

func TestCorruptCAFilesRejected(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "ca-cert.pem"), []byte("not pem"), 0o644)
	os.WriteFile(filepath.Join(dir, "ca-key.pem"), []byte("not pem"), 0o600)
	if _, err := LoadOrCreate(dir); err == nil {
		t.Fatal("accepted corrupt CA files")
	}
}
