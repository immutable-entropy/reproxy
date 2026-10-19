// Package tlsca is the proxy's certificate authority for HTTPS interception.
//
// On first use it generates a CA (a private key and a self-signed root
// certificate) and stores it in the data directory. For each host the browser
// connects to over TLS, the proxy mints a short-lived leaf certificate signed
// by that CA, so it can terminate the browser's TLS connection, read and edit
// the traffic, and open a fresh TLS connection to the real server.
//
// The browser only trusts these leaf certificates if the CA certificate has
// been installed as trusted. That is why the CA certificate is downloadable
// from the settings page and installed into the embedded browser's profile.
//
// It is core (intercept/edit/replay; see CLAUDE.md) and has no Qt dependency.
package tlsca

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"reproxy/internal/atomicfile"
)

// caValidity is how long a freshly generated CA certificate is valid. It is
// long because reinstalling the CA in the browser is a manual step.
const caValidity = 10 * 365 * 24 * time.Hour

// leafValidity is how long each minted leaf certificate is valid. Leaves are
// cached in memory only and regenerated freely, so this is kept short.
const leafValidity = 7 * 24 * time.Hour

// CommonName is the name shown for the CA in a browser's certificate manager.
const CommonName = "reProxy CA"

// CA is the proxy's certificate authority and its leaf-certificate cache.
// It is safe for concurrent use.
type CA struct {
	cert    *x509.Certificate
	key     *ecdsa.PrivateKey
	certPEM []byte

	mu     sync.Mutex
	leaves map[string]*tls.Certificate
}

// LoadOrCreate loads the CA from dir, generating and saving a new one if dir
// holds none. The private key is written with owner-only permissions.
func LoadOrCreate(dir string) (*CA, error) {
	certPath, keyPath := filepath.Join(dir, "ca-cert.pem"), filepath.Join(dir, "ca-key.pem")

	certPEM, certErr := os.ReadFile(certPath)
	keyPEM, keyErr := os.ReadFile(keyPath)
	switch {
	case certErr == nil && keyErr == nil:
		return load(certPEM, keyPEM)
	case errors.Is(certErr, fs.ErrNotExist) && errors.Is(keyErr, fs.ErrNotExist):
		return create(dir, certPath, keyPath)
	default:
		return nil, fmt.Errorf("reading CA: %w", errors.Join(certErr, keyErr))
	}
}

func load(certPEM, keyPEM []byte) (*CA, error) {
	certBlock, _ := pem.Decode(certPEM)
	keyBlock, _ := pem.Decode(keyPEM)
	if certBlock == nil || keyBlock == nil {
		return nil, errors.New("CA files are not valid PEM")
	}
	cert, err := x509.ParseCertificate(certBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing CA certificate: %w", err)
	}
	key, err := x509.ParseECPrivateKey(keyBlock.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing CA key: %w", err)
	}
	return &CA{cert: cert, key: key, certPEM: certPEM, leaves: map[string]*tls.Certificate{}}, nil
}

func create(dir, certPath, keyPath string) (*CA, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: CommonName, Organization: []string{"reProxy"}},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(caValidity),
		KeyUsage:     x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		IsCA:         true,
		// Leaf certs chain to this root and nothing else, so a path length of
		// zero (no intermediate CAs) is correct.
		BasicConstraintsValid: true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, err
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})

	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("creating CA directory: %w", err)
	}
	// The key is the CA's: anyone who reads it can mint a certificate the
	// user's browsers trust. The certificate is public by design.
	if err := atomicfile.Write(keyPath, keyPEM, atomicfile.Private, atomicfile.PrivateDir); err != nil {
		return nil, err
	}
	if err := atomicfile.Write(certPath, certPEM, 0o644, atomicfile.PrivateDir); err != nil {
		return nil, err
	}
	return &CA{cert: cert, key: key, certPEM: certPEM, leaves: map[string]*tls.Certificate{}}, nil
}

// CertPEM returns the CA certificate in PEM form, for download and for
// installing into a browser's trust store.
func (c *CA) CertPEM() []byte {
	out := make([]byte, len(c.certPEM))
	copy(out, c.certPEM)
	return out
}

// LeafFor returns a certificate for host, signed by the CA, minting and
// caching one on first request. host is a DNS name or an IP address.
func (c *CA) LeafFor(host string) (*tls.Certificate, error) {
	host = stripPort(host)

	c.mu.Lock()
	if leaf, ok := c.leaves[host]; ok {
		c.mu.Unlock()
		return leaf, nil
	}
	c.mu.Unlock()

	leaf, err := c.mint(host)
	if err != nil {
		return nil, err
	}

	c.mu.Lock()
	c.leaves[host] = leaf
	c.mu.Unlock()
	return leaf, nil
}

func (c *CA) mint(host string) (*tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	serial, err := randomSerial()
	if err != nil {
		return nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    now.Add(-time.Hour),
		NotAfter:     now.Add(leafValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		tmpl.IPAddresses = []net.IP{ip}
	} else {
		tmpl.DNSNames = []string{host}
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, c.cert, &key.PublicKey, c.key)
	if err != nil {
		return nil, err
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, err
	}
	return &tls.Certificate{
		Certificate: [][]byte{der, c.cert.Raw},
		PrivateKey:  key,
		Leaf:        leaf,
	}, nil
}

// randomSerial returns a positive 128-bit serial number.
func randomSerial() (*big.Int, error) {
	return rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
}

func stripPort(host string) string {
	if h, _, err := net.SplitHostPort(host); err == nil {
		return h
	}
	return host
}

// Fingerprint returns the SHA-256 fingerprint of the CA certificate, as
// pairs of hex digits separated by colons, for display.
func (c *CA) Fingerprint() string {
	sum := sha256.Sum256(c.cert.Raw)
	const hexdigits = "0123456789ABCDEF"
	out := make([]byte, 0, len(sum)*3-1)
	for i, b := range sum {
		if i > 0 {
			out = append(out, ':')
		}
		out = append(out, hexdigits[b>>4], hexdigits[b&0xf])
	}
	return string(out)
}
