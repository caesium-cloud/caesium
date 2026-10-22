package cliutil

import (
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// The CLI credential store (identity-and-access E5, design §13.1 step 6):
// `caesium login` writes the user-bound key it obtained to
// $XDG_CONFIG_HOME/caesium/credentials.yaml (~/.config when XDG_CONFIG_HOME is
// unset), keyed by server URL. The file is 0600 inside a 0700 directory and is
// refused when group or others can read it. The plaintext key is the bearer
// later commands present; it is never logged or printed.

const (
	credentialsDirName  = "caesium"
	credentialsFileName = "credentials.yaml"
	// PendingLoginTTL bounds how long `caesium login --no-browser` keeps the
	// verifier for the follow-up `caesium login --code`.
	PendingLoginTTL = 5 * time.Minute
)

// StoredCredential is one server's stored login.
type StoredCredential struct {
	Server    string    `yaml:"server"`
	Key       string    `yaml:"key"`
	KeyID     string    `yaml:"key_id"`
	KeyPrefix string    `yaml:"key_prefix"`
	ExpiresAt time.Time `yaml:"expires_at"`
	Subject   string    `yaml:"subject"`
}

// Expired reports whether the credential's expiry has passed.
func (c *StoredCredential) Expired(now time.Time) bool {
	return !c.ExpiresAt.IsZero() && !now.Before(c.ExpiresAt)
}

// PendingLogin is a `caesium login --no-browser` awaiting its code.
type PendingLogin struct {
	Server    string    `yaml:"server"`
	State     string    `yaml:"state"`
	Verifier  string    `yaml:"verifier"`
	ExpiresAt time.Time `yaml:"expires_at"`
}

type credentialFile struct {
	Credentials []StoredCredential `yaml:"credentials"`
	Pending     []PendingLogin     `yaml:"pending,omitempty"`
}

// CredentialStore is the loaded credentials file.
type CredentialStore struct {
	path string
	file credentialFile
}

// CredentialsDir is $XDG_CONFIG_HOME/caesium, or ~/.config/caesium when
// XDG_CONFIG_HOME is unset or not absolute.
func CredentialsDir() (string, error) {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" || !filepath.IsAbs(base) {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("locate home directory: %w", err)
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, credentialsDirName), nil
}

// CredentialsPath is the credentials file's path.
func CredentialsPath() (string, error) {
	dir, err := CredentialsDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, credentialsFileName), nil
}

// CanonicalServer is the key a server URL is stored under: lower-case scheme
// and host, no trailing slash, no query or fragment.
func CanonicalServer(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return "", fmt.Errorf("server %q must be an absolute http(s) URL", raw)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("server %q must be an absolute http(s) URL", raw)
	}
	return scheme + "://" + strings.ToLower(u.Host) + strings.TrimRight(u.Path, "/"), nil
}

// LoadCredentials reads the credentials file; a missing file is an empty
// store. A file group or others can access is refused.
func LoadCredentials() (*CredentialStore, error) {
	path, err := CredentialsPath()
	if err != nil {
		return nil, err
	}
	store := &CredentialStore{path: path}
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return store, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("credentials file %s is accessible by other users (mode %04o); run chmod 600 on it", path, info.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read credentials: %w", err)
	}
	if err := yaml.Unmarshal(raw, &store.file); err != nil {
		return nil, fmt.Errorf("parse credentials file %s: %w", path, err)
	}
	return store, nil
}

// Path is where the store is saved.
func (s *CredentialStore) Path() string { return s.path }

// Lookup returns the stored credential for server.
func (s *CredentialStore) Lookup(server string) (*StoredCredential, bool) {
	key, err := CanonicalServer(server)
	if err != nil {
		return nil, false
	}
	for i := range s.file.Credentials {
		if s.file.Credentials[i].Server == key {
			c := s.file.Credentials[i]
			return &c, true
		}
	}
	return nil, false
}

// Put stores cred, replacing any entry for its server.
func (s *CredentialStore) Put(cred StoredCredential) error {
	key, err := CanonicalServer(cred.Server)
	if err != nil {
		return err
	}
	cred.Server = key
	s.Remove(key)
	s.file.Credentials = append(s.file.Credentials, cred)
	return nil
}

// Remove deletes server's entry and reports whether there was one.
func (s *CredentialStore) Remove(server string) bool {
	key, err := CanonicalServer(server)
	if err != nil {
		return false
	}
	kept := s.file.Credentials[:0]
	removed := false
	for _, c := range s.file.Credentials {
		if c.Server == key {
			removed = true
			continue
		}
		kept = append(kept, c)
	}
	s.file.Credentials = kept
	return removed
}

// PutPending records a pending `--no-browser` login for its server,
// replacing any earlier one, and drops expired entries.
func (s *CredentialStore) PutPending(p PendingLogin, now time.Time) error {
	key, err := CanonicalServer(p.Server)
	if err != nil {
		return err
	}
	p.Server = key
	s.takePending(key, now)
	s.file.Pending = append(s.file.Pending, p)
	return nil
}

// PendingServers lists the servers with an unexpired pending login.
func (s *CredentialStore) PendingServers(now time.Time) []string {
	var servers []string
	for _, p := range s.file.Pending {
		if now.Before(p.ExpiresAt) {
			servers = append(servers, p.Server)
		}
	}
	return servers
}

// TakePending removes and returns server's unexpired pending login.
func (s *CredentialStore) TakePending(server string, now time.Time) (*PendingLogin, bool) {
	key, err := CanonicalServer(server)
	if err != nil {
		return nil, false
	}
	return s.takePending(key, now)
}

func (s *CredentialStore) takePending(key string, now time.Time) (*PendingLogin, bool) {
	var found *PendingLogin
	kept := s.file.Pending[:0]
	for _, p := range s.file.Pending {
		if !now.Before(p.ExpiresAt) {
			continue
		}
		if p.Server == key {
			p := p
			found = &p
			continue
		}
		kept = append(kept, p)
	}
	s.file.Pending = kept
	return found, found != nil
}

// UpdateCredentials applies fn to the freshly loaded store and saves it,
// holding an exclusive lock on credentials.lock (beside the file) from the
// load to the save, so concurrent CLI processes (a login for one server, a
// logout for another) never overwrite each other's entries. fn must not do
// network I/O: the lock is held while it runs.
func UpdateCredentials(fn func(*CredentialStore) error) error {
	dir, err := CredentialsDir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure credentials directory: %w", err)
	}
	lock, err := os.OpenFile(filepath.Join(dir, "credentials.lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("lock credentials: %w", err)
	}
	defer func() { _ = lock.Close() }()
	if err := lockFile(lock); err != nil {
		return fmt.Errorf("lock credentials: %w", err)
	}
	defer func() { _ = unlockFile(lock) }()
	store, err := LoadCredentials()
	if err != nil {
		return err
	}
	if err := fn(store); err != nil {
		return err
	}
	return store.Save()
}

// Save writes the store atomically: the directory is created (and kept) at
// 0700, the file is written to a 0600 temporary file and renamed over the
// old one.
func (s *CredentialStore) Save() error {
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create credentials directory: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("secure credentials directory: %w", err)
	}
	raw, err := yaml.Marshal(&s.file)
	if err != nil {
		return fmt.Errorf("encode credentials: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".credentials-*.yaml")
	if err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("secure credentials: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("write credentials: %w", err)
	}
	return nil
}
