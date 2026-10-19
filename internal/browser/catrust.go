package browser

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"reproxy/internal/tlsca"
)

// errNoCertutil means the certutil tool (from the nss / libnss3-tools
// package) isn't installed, so the CA can't be added to the browser's trust
// store automatically.
var errNoCertutil = errors.New("certutil not found (install libnss3-tools / nss) to trust the proxy CA automatically")

// installCA adds caPEM to the trust store the browser reads, so the proxy's
// minted certificates are accepted without warnings. It uses certutil to
// manage the NSS database that Chromium and Firefox use on Linux:
//
//   - Firefox reads the cert database inside its own profile.
//   - Chromium reads the shared per-user database at ~/.pki/nssdb, not the
//     profile, so the CA is added there (and is visible to the user's other
//     Chromium profiles; it is named so it can be found and removed).
//
// It is idempotent: an existing entry with the same name is replaced.
func installCA(b Browser, profile string, caPEM []byte) error {
	certutil, err := exec.LookPath("certutil")
	if err != nil {
		return errNoCertutil
	}

	dbDir, err := nssDBDir(b, profile)
	if err != nil {
		return err
	}
	if err := ensureNSSDB(certutil, dbDir); err != nil {
		return err
	}

	pemFile, err := os.CreateTemp("", "reproxy-ca-*.pem")
	if err != nil {
		return err
	}
	defer os.Remove(pemFile.Name())
	if _, err := pemFile.Write(caPEM); err != nil {
		pemFile.Close()
		return err
	}
	pemFile.Close()

	db := "sql:" + dbDir
	// Replace any previous copy so a regenerated CA takes effect.
	_ = run(certutil, "-D", "-d", db, "-n", tlsca.CommonName)
	if err := run(certutil, "-A", "-d", db, "-n", tlsca.CommonName, "-t", "C,,", "-i", pemFile.Name()); err != nil {
		return fmt.Errorf("adding CA to %s: %w", dbDir, err)
	}
	return nil
}

// nssDBDir returns the NSS database directory for b.
func nssDBDir(b Browser, profile string) (string, error) {
	if b.Kind == Firefox {
		return profile, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pki", "nssdb"), nil
}

// ensureNSSDB creates an empty NSS database in dir if none exists.
func ensureNSSDB(certutil, dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(dir, "cert9.db")); err == nil {
		return nil
	}
	return run(certutil, "-N", "-d", "sql:"+dir, "--empty-password")
}

func run(name string, args ...string) error {
	cmd := exec.Command(name, args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s: %w: %s", name, err, out)
	}
	return nil
}
