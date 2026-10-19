// Package secrets keeps credentials out of the settings file: agent API keys
// and the MCP server's bearer token live in secrets.json beside config.json,
// readable only by the owner (like the CA key).
//
// Every running reproxy process shares the file, so each read goes to disk
// and each write is a read-modify-write under an in-process lock, written
// atomically. Two processes writing at the same instant could still race;
// writes are rare (the user editing keys in Settings), so that is accepted.
package secrets

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"reproxy/internal/atomicfile"
	"reproxy/internal/randid"
)

// FileName is the secrets file's name inside the data directory.
const FileName = "secrets.json"

type file struct {
	MCPToken string            `json:"mcp_token,omitempty"`
	APIKeys  map[string]string `json:"api_keys,omitempty"` // by agent profile ID
	// ProxyAuth holds upstream proxy passwords, by upstream.Proxy ID.
	ProxyAuth map[string]string `json:"proxy_auth,omitempty"`
	// Plugins holds plugins' secret settings, by plugin name then key.
	Plugins map[string]map[string]string `json:"plugins,omitempty"`
}

// Store reads and writes one secrets file.
type Store struct {
	path string
	mu   sync.Mutex
}

// New returns a Store for the secrets file in dataDir. Nothing is read or
// created until it is used.
func New(dataDir string) *Store {
	return &Store{path: filepath.Join(dataDir, FileName)}
}

// Path returns the file's location.
func (s *Store) Path() string { return s.path }

// APIKey returns the key saved for an agent profile, or "".
func (s *Store) APIKey(profileID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	return f.APIKeys[profileID], err
}

// SetAPIKey saves (or, with key == "", removes) a profile's key.
func (s *Store) SetAPIKey(profileID, key string) error {
	return s.update(func(f *file) {
		if key == "" {
			delete(f.APIKeys, profileID)
			return
		}
		if f.APIKeys == nil {
			f.APIKeys = make(map[string]string)
		}
		f.APIKeys[profileID] = key
	})
}

// ProxyPassword returns the password saved for an upstream proxy, or "".
func (s *Store) ProxyPassword(proxyID string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	return f.ProxyAuth[proxyID], err
}

// UpdateProxyPasswords saves the passwords given, by proxy ID, and removes
// those given as "". Passwords not mentioned are left alone: the global
// chain and each engagement's own chain keep theirs in the same file, so
// saving one must not touch the others'.
func (s *Store) UpdateProxyPasswords(byID map[string]string) error {
	return s.update(func(f *file) {
		for id, pw := range byID {
			if pw == "" {
				delete(f.ProxyAuth, id)
				continue
			}
			if f.ProxyAuth == nil {
				f.ProxyAuth = make(map[string]string, len(byID))
			}
			f.ProxyAuth[id] = pw
		}
	})
}

// SetProxyPasswords replaces every saved upstream proxy password with all,
// so a proxy removed from the chain loses its password too. An entry with
// an empty password is dropped rather than stored.
func (s *Store) SetProxyPasswords(all map[string]string) error {
	return s.update(func(f *file) {
		f.ProxyAuth = nil
		for id, pw := range all {
			if pw == "" {
				continue
			}
			if f.ProxyAuth == nil {
				f.ProxyAuth = make(map[string]string, len(all))
			}
			f.ProxyAuth[id] = pw
		}
	})
}

// PluginSecret returns a plugin's secret setting, or "".
func (s *Store) PluginSecret(plugin, key string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	return f.Plugins[plugin][key], err
}

// SetPluginSecrets saves a plugin's secret settings. An empty value removes
// that setting's secret; keys not given are left alone.
func (s *Store) SetPluginSecrets(plugin string, values map[string]string) error {
	return s.update(func(f *file) {
		for key, v := range values {
			if v == "" {
				delete(f.Plugins[plugin], key)
				continue
			}
			if f.Plugins == nil {
				f.Plugins = make(map[string]map[string]string)
			}
			if f.Plugins[plugin] == nil {
				f.Plugins[plugin] = make(map[string]string)
			}
			f.Plugins[plugin][key] = v
		}
		if len(f.Plugins[plugin]) == 0 {
			delete(f.Plugins, plugin)
		}
	})
}

// MCPToken returns the bearer token that MCP clients and other reproxy
// processes must present, generating and saving one on first use.
func (s *Store) MCPToken() (string, error) {
	s.mu.Lock()
	f, err := s.read()
	s.mu.Unlock()
	if err != nil {
		return "", err
	}
	if f.MCPToken != "" {
		return f.MCPToken, nil
	}
	var tok string
	err = s.update(func(f *file) {
		if f.MCPToken == "" { // another caller may have won the race
			f.MCPToken = newToken()
		}
		tok = f.MCPToken
	})
	return tok, err
}

// RegenerateMCPToken replaces the token; clients using the old one stop
// working.
func (s *Store) RegenerateMCPToken() (string, error) {
	tok := newToken()
	return tok, s.update(func(f *file) { f.MCPToken = tok })
}

func newToken() string { return "rpx_" + randid.Hex(32) }

func (s *Store) update(mutate func(*file)) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := s.read()
	if err != nil {
		return err
	}
	mutate(&f)
	return s.write(f)
}

func (s *Store) read() (file, error) {
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return file{}, nil
	}
	if err != nil {
		return file{}, fmt.Errorf("reading secrets: %w", err)
	}
	var f file
	if err := json.Unmarshal(data, &f); err != nil {
		return file{}, fmt.Errorf("parsing %s: %w", s.path, err)
	}
	return f, nil
}

func (s *Store) write(f file) error {
	if err := atomicfile.WriteJSON(s.path, f, atomicfile.Private, 0o755); err != nil {
		return fmt.Errorf("writing secrets: %w", err)
	}
	return nil
}
