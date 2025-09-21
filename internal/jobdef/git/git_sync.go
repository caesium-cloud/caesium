package git

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/caesium-cloud/caesium/internal/jobdef"
	"github.com/caesium-cloud/caesium/internal/jobdef/secret"
	schema "github.com/caesium-cloud/caesium/pkg/jobdef"
	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/transport"
	httpauth "github.com/go-git/go-git/v5/plumbing/transport/http"
	sshauth "github.com/go-git/go-git/v5/plumbing/transport/ssh"
	"github.com/skeema/knownhosts"
	"gopkg.in/yaml.v3"
)

// Source contains the parameters required to pull job definitions from Git.
type Source struct {
	URL      string
	Ref      string
	Path     string
	Auth     *BasicAuth
	SSH      *SSHAuth
	Resolver secret.Resolver
	SourceID string
	Globs    []string
	LocalDir string
}

// BasicAuth holds optional credentials for HTTPS remotes.
type BasicAuth struct {
	Username    string
	Password    string
	UsernameRef string
	PasswordRef string
}

// SSHAuth holds configuration for SSH-based authentication.
type SSHAuth struct {
	Username        string
	UsernameRef     string
	PrivateKey      string
	PrivateKeyRef   string
	Passphrase      string
	PassphraseRef   string
	KnownHosts      string
	KnownHostsRef   string
	KnownHostsPath  string
	KnownHostsPaths []string
}

// Sync clones/fetches the repository and applies manifests via the importer.
func (s *Source) Sync(ctx context.Context, importer *jobdef.Importer) error {
	dir, hash, cleanup, err := s.cloneRepo(ctx)
	if err != nil {
		return err
	}
	defer cleanup()

	return s.applyDir(ctx, importer, dir, hash)
}

// WatchOptions configure a recurring sync loop.
type WatchOptions struct {
	Source   Source
	Interval time.Duration
	Once     bool
}

// Watch performs an initial sync and optionally continues on an interval until ctx is cancelled.
func Watch(ctx context.Context, importer *jobdef.Importer, opts WatchOptions) error {
	if importer == nil {
		return errors.New("importer is required")
	}
	if opts.Interval <= 0 {
		opts.Interval = time.Minute
	}

	cloneDir := strings.TrimSpace(opts.Source.LocalDir)
	cleanup := func() {}
	if cloneDir == "" {
		dir, err := os.MkdirTemp("", "caesium-jobdef-watch-")
		if err != nil {
			return err
		}
		cloneDir = dir
		cleanup = func() {
			if err := os.RemoveAll(cloneDir); err != nil {
				log.Error("cleanup watch clone", "dir", cloneDir, "error", err)
			}
		}
	}
	defer cleanup()

	var repo *git.Repository
	lastHash := ""

	syncOnce := func() error {
		cloneOpts, authCleanup, err := opts.Source.cloneOptions(ctx)
		if err != nil {
			return err
		}
		defer authCleanup()

		repo, err = ensureRepo(ctx, cloneDir, cloneOpts, repo)
		if err != nil {
			return err
		}

		hash, err := headHash(repo)
		if err != nil {
			return err
		}

		if hash == lastHash {
			return nil
		}

		if err := opts.Source.applyDir(ctx, importer, cloneDir, hash); err != nil {
			return err
		}

		lastHash = hash
		return nil
	}

	if err := syncOnce(); err != nil {
		return err
	}

	if opts.Once {
		return nil
	}

	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return context.Cause(ctx)
		case <-ticker.C:
			if err := syncOnce(); err != nil {
				return err
			}
		}
	}
}

func (s *Source) cloneRepo(ctx context.Context) (string, string, func(), error) {
	dir, err := os.MkdirTemp("", "caesium-jobdef-")
	if err != nil {
		return "", "", nil, err
	}

	cloneOpts, authCleanup, err := s.cloneOptions(ctx)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", nil, err
	}
	defer authCleanup()

	repo, err := ensureRepo(ctx, dir, cloneOpts, nil)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", nil, err
	}

	hash, err := headHash(repo)
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", "", nil, err
	}

	cleanup := func() {
		if err := os.RemoveAll(dir); err != nil {
			log.Error("cleanup clone dir", "dir", dir, "error", err)
		}
	}
	return dir, hash, cleanup, nil
}

func (s *Source) applyDir(ctx context.Context, importer *jobdef.Importer, dir string, commit string) error {
	root := filepath.Join(dir, strings.TrimPrefix(s.Path, "/"))
	if strings.TrimSpace(s.Path) == "" {
		root = dir
	}

	if _, err := os.Stat(root); err != nil {
		return err
	}

	var files []string
	if err := filepath.WalkDir(root, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}

		relToRoot, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}

		if !s.shouldInclude(path, relToRoot) {
			return nil
		}

		files = append(files, path)
		return nil
	}); err != nil {
		return err
	}

	sort.Strings(files)

	for _, path := range files {
		relToRepo, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		prov := &jobdef.Provenance{
			SourceID: strings.TrimSpace(s.SourceID),
			Repo:     s.URL,
			Ref:      s.Ref,
			Commit:   commit,
			Path:     filepath.ToSlash(relToRepo),
		}

		opts := &jobdef.ApplyOptions{Provenance: prov}
		if err := applyFile(ctx, importer, path, opts); err != nil {
			return err
		}
	}

	return nil
}

func (s *Source) cloneOptions(ctx context.Context) (*git.CloneOptions, func(), error) {
	auth, cleanup, err := s.authMethod(ctx)
	if err != nil {
		return nil, cleanup, err
	}
	if cleanup == nil {
		cleanup = func() {}
	}

	return &git.CloneOptions{
		URL:           s.URL,
		Depth:         1,
		SingleBranch:  true,
		ReferenceName: referenceNameOrDefault(s.Ref),
		Auth:          auth,
	}, cleanup, nil
}

func (s *Source) authMethod(ctx context.Context) (transport.AuthMethod, func(), error) {
	noop := func() {}
	if s.SSH != nil {
		auth, cleanup, err := s.sshAuth(ctx)
		if cleanup == nil {
			cleanup = noop
		}
		return auth, cleanup, err
	}
	if s.Auth != nil {
		auth, err := s.basicAuth(ctx)
		return auth, noop, err
	}
	return nil, noop, nil
}

func (s *Source) basicAuth(ctx context.Context) (*httpauth.BasicAuth, error) {
	if s.Auth == nil {
		return nil, nil
	}

	username := strings.TrimSpace(s.Auth.Username)
	password := s.Auth.Password

	if username == "" && strings.TrimSpace(s.Auth.UsernameRef) != "" {
		value, err := s.resolveSecret(ctx, s.Auth.UsernameRef)
		if err != nil {
			return nil, err
		}
		username = strings.TrimSpace(value)
	}

	if password == "" && strings.TrimSpace(s.Auth.PasswordRef) != "" {
		value, err := s.resolveSecret(ctx, s.Auth.PasswordRef)
		if err != nil {
			return nil, err
		}
		password = value
	}

	if username == "" && strings.TrimSpace(password) == "" {
		return nil, nil
	}

	return &httpauth.BasicAuth{Username: username, Password: password}, nil
}

func (s *Source) sshAuth(ctx context.Context) (transport.AuthMethod, func(), error) {
	noop := func() {}
	if s.SSH == nil {
		return nil, noop, nil
	}

	username := strings.TrimSpace(s.SSH.Username)
	if username == "" && strings.TrimSpace(s.SSH.UsernameRef) != "" {
		value, err := s.resolveSecret(ctx, s.SSH.UsernameRef)
		if err != nil {
			return nil, noop, err
		}
		username = strings.TrimSpace(value)
	}
	if username == "" {
		username = sshauth.DefaultUsername
	}

	privateKey := s.SSH.PrivateKey
	if strings.TrimSpace(privateKey) == "" && strings.TrimSpace(s.SSH.PrivateKeyRef) != "" {
		value, err := s.resolveSecret(ctx, s.SSH.PrivateKeyRef)
		if err != nil {
			return nil, noop, err
		}
		privateKey = value
	}
	if strings.TrimSpace(privateKey) == "" {
		return nil, noop, errors.New("ssh private key is required")
	}

	passphrase := s.SSH.Passphrase
	if passphrase == "" && strings.TrimSpace(s.SSH.PassphraseRef) != "" {
		value, err := s.resolveSecret(ctx, s.SSH.PassphraseRef)
		if err != nil {
			return nil, noop, err
		}
		passphrase = value
	}

	pk, err := sshauth.NewPublicKeys(username, []byte(privateKey), passphrase)
	if err != nil {
		return nil, noop, err
	}

	endpoint, err := transport.NewEndpoint(s.URL)
	if err != nil {
		return nil, noop, err
	}

	helper, cleanup, err := s.sshKnownHosts(ctx, endpoint)
	if err != nil {
		return nil, noop, err
	}
	pk.HostKeyCallbackHelper = helper

	return pk, cleanup, nil
}

func (s *Source) sshKnownHosts(ctx context.Context, endpoint *transport.Endpoint) (sshauth.HostKeyCallbackHelper, func(), error) {
	noopCleanup := func() {}
	if s.SSH == nil {
		return sshauth.HostKeyCallbackHelper{}, noopCleanup, nil
	}

	paths := make([]string, 0, len(s.SSH.KnownHostsPaths)+1)
	seen := make(map[string]struct{})

	addPath := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if _, ok := seen[p]; ok {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}

	for _, p := range s.SSH.KnownHostsPaths {
		addPath(p)
	}
	addPath(s.SSH.KnownHostsPath)

	data := strings.TrimSpace(s.SSH.KnownHosts)
	if data == "" && strings.TrimSpace(s.SSH.KnownHostsRef) != "" {
		value, err := s.resolveSecret(ctx, s.SSH.KnownHostsRef)
		if err != nil {
			return sshauth.HostKeyCallbackHelper{}, noopCleanup, err
		}
		data = strings.TrimSpace(value)
	}

	cleanup := noopCleanup
	if data != "" {
		file, err := os.CreateTemp("", "caesium-known-hosts-")
		if err != nil {
			return sshauth.HostKeyCallbackHelper{}, noopCleanup, err
		}
		if !strings.HasSuffix(data, "\n") {
			data += "\n"
		}
		if _, err := file.WriteString(data); err != nil {
			if closeErr := file.Close(); closeErr != nil {
				err = errors.Join(err, closeErr)
			}
			if removeErr := os.Remove(file.Name()); removeErr != nil {
				log.Error("remove temp known_hosts", "file", file.Name(), "error", removeErr)
			}
			return sshauth.HostKeyCallbackHelper{}, noopCleanup, err
		}
		if err := file.Close(); err != nil {
			if removeErr := os.Remove(file.Name()); removeErr != nil {
				log.Error("remove temp known_hosts", "file", file.Name(), "error", removeErr)
			}
			return sshauth.HostKeyCallbackHelper{}, noopCleanup, err
		}
		addPath(file.Name())
		cleanup = func() {
			if err := os.Remove(file.Name()); err != nil {
				log.Error("remove temp known_hosts", "file", file.Name(), "error", err)
			}
		}
	}

	if len(paths) == 0 {
		return sshauth.HostKeyCallbackHelper{}, noopCleanup, errors.New("ssh known hosts configuration required")
	}

	db, err := knownhosts.NewDB(paths...)
	if err != nil {
		cleanup()
		return sshauth.HostKeyCallbackHelper{}, noopCleanup, err
	}

	host := hostWithPort(endpoint)
	if host != "" && len(db.HostKeyAlgorithms(host)) == 0 {
		cleanup()
		return sshauth.HostKeyCallbackHelper{}, noopCleanup, fmt.Errorf("no known_hosts entry for %s", host)
	}

	helper := sshauth.HostKeyCallbackHelper{
		HostKeyCallback: db.HostKeyCallback(),
	}

	return helper, cleanup, nil
}

func (s *Source) resolveSecret(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", nil
	}
	if s.Resolver == nil {
		return "", fmt.Errorf("secret resolver not configured for %q", ref)
	}
	value, err := s.Resolver.Resolve(ctx, ref)
	if err != nil {
		return "", fmt.Errorf("resolve secret %q: %w", ref, err)
	}
	return value, nil
}

func hostWithPort(endpoint *transport.Endpoint) string {
	if endpoint == nil {
		return ""
	}
	host := strings.TrimSpace(endpoint.Host)
	if host == "" {
		return ""
	}
	port := endpoint.Port
	if port == 0 {
		switch strings.ToLower(endpoint.Protocol) {
		case "ssh", "git+ssh", "scp":
			port = 22
		case "http":
			port = 80
		case "https":
			port = 443
		case "git":
			port = 9418
		default:
			port = 22
		}
	}
	if strings.Contains(host, ":") && !strings.HasPrefix(host, "[") {
		host = fmt.Sprintf("[%s]", host)
	}
	return fmt.Sprintf("%s:%d", host, port)
}

func ensureRepo(ctx context.Context, dir string, opts *git.CloneOptions, repo *git.Repository) (*git.Repository, error) {
	if repo == nil {
		_ = os.RemoveAll(dir)
		return git.PlainCloneContext(ctx, dir, false, opts)
	}

	wt, err := repo.Worktree()
	if err != nil {
		return nil, err
	}

	branch := opts.ReferenceName
	if branch == "" {
		branch = referenceNameOrDefault("")
	}

	pullOpts := &git.PullOptions{
		RemoteName:    "origin",
		ReferenceName: branch,
		Auth:          opts.Auth,
		SingleBranch:  true,
		Force:         true,
	}
	err = wt.PullContext(ctx, pullOpts)
	switch {
	case err == nil, err == git.NoErrAlreadyUpToDate:
		return repo, nil
	case err == git.ErrNonFastForwardUpdate:
		remoteRef := plumbing.NewRemoteReferenceName("origin", branch.Short())
		ref, refErr := repo.Reference(remoteRef, true)
		if refErr != nil {
			return nil, refErr
		}
		if resetErr := wt.Reset(&git.ResetOptions{Mode: git.HardReset, Commit: ref.Hash()}); resetErr != nil {
			return nil, resetErr
		}
		return repo, nil
	case errors.Is(err, transport.ErrEmptyRemoteRepository):
		return repo, nil
	case errors.Is(err, plumbing.ErrReferenceNotFound):
		_ = os.RemoveAll(dir)
		return git.PlainCloneContext(ctx, dir, false, opts)
	default:
		return nil, err
	}
}

func headHash(repo *git.Repository) (string, error) {
	ref, err := repo.Head()
	if err != nil {
		return "", err
	}
	return ref.Hash().String(), nil
}

func referenceNameOrDefault(ref string) plumbing.ReferenceName {
	if strings.TrimSpace(ref) == "" {
		return plumbing.NewBranchReferenceName("main")
	}
	if strings.HasPrefix(ref, "refs/") {
		return plumbing.ReferenceName(ref)
	}
	return plumbing.NewBranchReferenceName(ref)
}

func isYAML(path string) bool {
	ext := strings.ToLower(filepath.Ext(path))
	return ext == ".yaml" || ext == ".yml"
}

func (s *Source) shouldInclude(fullPath, relative string) bool {
	if len(s.Globs) == 0 {
		return isYAML(fullPath)
	}
	rel := filepath.ToSlash(relative)
	for _, pattern := range s.Globs {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			continue
		}
		match, err := doublestar.PathMatch(pattern, rel)
		if err != nil {
			continue
		}
		if match {
			return true
		}
	}
	return false
}

func applyFile(ctx context.Context, importer *jobdef.Importer, path string, opts *jobdef.ApplyOptions) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	dec := yamlNewDecoder(data)
	for {
		def, err := dec()
		if errors.Is(err, errEOF) {
			return nil
		}
		if err != nil {
			return err
		}

		if _, err := importer.ApplyWithOptions(ctx, def, opts); err != nil {
			return err
		}
	}
}

var errEOF = errors.New("eof")

func yamlNewDecoder(data []byte) func() (*schema.Definition, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(data))

	return func() (*schema.Definition, error) {
		var def schema.Definition
		if err := decoder.Decode(&def); err != nil {
			if errors.Is(err, io.EOF) {
				return nil, errEOF
			}
			return nil, err
		}
		return &def, nil
	}
}
