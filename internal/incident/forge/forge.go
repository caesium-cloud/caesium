// Package forge is the write side of a source repository: the handful of forge
// API calls that turn a rendered change into a pull request for
// apply_jobdef_patch's Git-PR route. It is an API client, not a git client;
// internal/jobdef/git stays the read-only sync path.
//
// The token is a server credential that a job must never be able to redirect.
// A Client sends it only to Config.APIBaseURL, follows no redirects, and scrubs
// it from every error it returns.
package forge

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
)

const (
	requestTimeout   = 15 * time.Second
	maxResponseBytes = 4 << 20
	maxMessageBytes  = 200
	patchBranchRoot  = "caesium/patch-"
)

var (
	// ErrNotFound reports that the named branch, file or commit does not exist.
	ErrNotFound = errors.New("forge: not found")
	// ErrConflict reports that the forge refused a write against the state it
	// holds: a stale blob sha, or a pull request that already exists.
	ErrConflict = errors.New("forge: conflict")
	// ErrFileNotEditable reports a path the contents API cannot round-trip: a
	// directory, or a file too large to be returned inline.
	ErrFileNotEditable = errors.New("forge: path is not an editable file")
	// ErrTooManyPullRequests reports a pull request lookup that reached its page
	// bound before it could choose.
	ErrTooManyPullRequests = errors.New("forge: too many pull requests")
)

// Repo names a repository on a forge. Host keeps a non-default http(s) port.
type Repo struct {
	Host  string
	Owner string
	Name  string
}

// String is the display form, which carries no URL and so no URL credential.
func (r Repo) String() string { return r.Owner + "/" + r.Name }

// File is one file as a forge stores it. SHA is its blob sha.
type File struct {
	SHA     string
	Content []byte
}

// PutFile writes one file on a branch. SHA is the blob being replaced, so the
// forge refuses the write if the file moved since it was read.
type PutFile struct {
	Path    string
	Branch  string
	Message string
	Content []byte
	SHA     string
}

// NewPullRequest opens a pull request from Head into Base.
type NewPullRequest struct {
	Title string
	Body  string
	Head  string
	Base  string
}

// PullRequest is a pull request as the route records it. Base and HeadSHA let the
// route check that a pull request it finds holds the change it would open: anyone
// who can push to the repository can open one from the route's branch. State is
// the forge's, "open" or "closed"; Merged tells a merged pull request from a
// rejected one, which the forge also reports as closed.
type PullRequest struct {
	Number  int
	URL     string
	State   string
	Merged  bool
	Base    string
	HeadSHA string
}

// Open reports whether the pull request is still open for review.
func (p *PullRequest) Open() bool { return p.State == "open" }

// Client is the forge surface the route needs. Each method is bound to one
// repository; a call cannot name another.
type Client interface {
	BranchHead(ctx context.Context, branch string) (string, error)
	GetFile(ctx context.Context, filePath, ref string) (*File, error)
	// CreateBranch reports false when the branch already exists.
	CreateBranch(ctx context.Context, branch, sha string) (bool, error)
	PutFile(ctx context.Context, file PutFile) error
	// FindPullRequest returns the pull request whose head is branch, or nil: an
	// open one into base, else a merged one, else any other (see the method).
	FindPullRequest(ctx context.Context, branch, base string) (*PullRequest, error)
	OpenPullRequest(ctx context.Context, pr NewPullRequest) (*PullRequest, error)
}

// Config binds a client to one forge, repository and token. Repo comes from
// server configuration, never from job data.
type Config struct {
	Kind       string
	APIBaseURL string
	Token      string
	Repo       Repo
}

// New builds the client for cfg.Kind. Every kind must honour the package
// contract on the token.
func New(cfg Config) (Client, error) {
	if cfg.Kind != env.GitForgeGitHub {
		return nil, fmt.Errorf("forge: unsupported kind %q", cfg.Kind)
	}
	if err := env.ValidateForgeAPIBaseURL(cfg.APIBaseURL); err != nil {
		return nil, fmt.Errorf("forge: %w", err)
	}
	if cfg.Token == "" {
		return nil, errors.New("forge: token is empty")
	}
	base, err := url.Parse(strings.TrimRight(strings.TrimSpace(cfg.APIBaseURL), "/"))
	if err != nil {
		return nil, errors.New("forge: api_base_url is not a URL")
	}
	if !apiServes(base, cfg.Repo.Host) {
		return nil, fmt.Errorf("forge: api_base_url does not serve repository host %q", cfg.Repo.Host)
	}
	return &github{
		base:  base,
		token: cfg.Token,
		repo:  cfg.Repo,
		http: &http.Client{
			Timeout: requestTimeout,
			// A redirect is an error: the token never leaves the configured API.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// apiServes reports whether an API root belongs to a repository's host:
// api.github.com serves github.com, and an enterprise API lives on its own host.
func apiServes(base *url.URL, repoHost string) bool {
	if base.Host == "api.github.com" {
		return repoHost == "github.com"
	}
	return strings.EqualFold(base.Host, repoHost)
}

var repoSegment = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// ParseRepoURL reads a clone URL (https, http, ssh, or scp-style
// git@host:owner/name) into a Repo. User info is dropped, and exactly
// owner/name is accepted.
func ParseRepoURL(raw string) (Repo, error) {
	raw = strings.TrimSpace(raw)
	if !strings.Contains(raw, "://") {
		host, repoPath, ok := strings.Cut(raw, ":")
		if !ok || strings.Contains(host, "/") {
			return Repo{}, errors.New("forge: not a repository URL")
		}
		if at := strings.LastIndex(host, "@"); at >= 0 {
			host = host[at+1:]
		}
		return newRepo(host, repoPath)
	}
	parsed, err := url.Parse(raw)
	if err != nil {
		return Repo{}, errors.New("forge: not a repository URL")
	}
	host := parsed.Hostname()
	switch parsed.Scheme {
	case "https":
		if port := parsed.Port(); port != "" && port != "443" {
			host = parsed.Host
		}
	case "http":
		if port := parsed.Port(); port != "" && port != "80" {
			host = parsed.Host
		}
	case "ssh", "git+ssh":
	default:
		return Repo{}, errors.New("forge: unsupported repository URL scheme")
	}
	return newRepo(host, parsed.Path)
}

func newRepo(host, repoPath string) (Repo, error) {
	segments := strings.Split(strings.Trim(repoPath, "/"), "/")
	if host == "" || len(segments) != 2 {
		return Repo{}, errors.New("forge: repository URL must name exactly owner/name")
	}
	owner, name := segments[0], strings.TrimSuffix(segments[1], ".git")
	for _, segment := range []string{owner, name} {
		if !repoSegment.MatchString(segment) || strings.Contains(segment, "..") {
			return Repo{}, errors.New("forge: repository URL has an invalid owner or name")
		}
	}
	return Repo{Host: strings.ToLower(host), Owner: owner, Name: name}, nil
}

// BranchForAction is the pull request branch of one approved action. It derives
// from the action id alone, so a repeated execution lands on the same branch and
// no job value reaches a ref name.
func BranchForAction(actionID uuid.UUID) string {
	return patchBranchRoot + actionID.String()
}

var hexSHA = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// ValidCommit reports whether sha is a full git object id.
func ValidCommit(sha string) bool { return hexSHA.MatchString(sha) }

// ValidateBranch applies git check-ref-format's rules to a branch name.
func ValidateBranch(name string) error {
	if name == "" || name == "@" || strings.HasPrefix(name, "-") || strings.HasPrefix(name, "/") ||
		strings.HasSuffix(name, "/") || strings.HasSuffix(name, ".") ||
		strings.Contains(name, "..") || strings.Contains(name, "//") || strings.Contains(name, "@{") {
		return errors.New("forge: invalid branch name")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f || strings.ContainsRune(" ~^:?*[\\", r) {
			return errors.New("forge: invalid branch name")
		}
	}
	for segment := range strings.SplitSeq(name, "/") {
		if strings.HasPrefix(segment, ".") || strings.HasSuffix(segment, ".lock") {
			return errors.New("forge: invalid branch name")
		}
	}
	return nil
}

// ValidatePath accepts a clean, relative, forward-slash repository path.
func ValidatePath(filePath string) error {
	if filePath == "" || path.IsAbs(filePath) || path.Clean(filePath) != filePath ||
		filePath == "." || filePath == ".." || strings.HasPrefix(filePath, "../") ||
		strings.Contains(filePath, "\\") {
		return errors.New("forge: invalid repository path")
	}
	for _, r := range filePath {
		if r < 0x20 || r == 0x7f {
			return errors.New("forge: invalid repository path")
		}
	}
	for segment := range strings.SplitSeq(filePath, "/") {
		if strings.EqualFold(segment, ".git") {
			return errors.New("forge: invalid repository path")
		}
	}
	return nil
}

// github speaks the GitHub REST API (and GitHub Enterprise's, under /api/v3).
type github struct {
	base  *url.URL
	token string
	repo  Repo
	http  *http.Client
}

func escapeSegments(value string) string {
	segments := strings.Split(value, "/")
	for i, segment := range segments {
		segments[i] = url.PathEscape(segment)
	}
	return strings.Join(segments, "/")
}

// repoPath is the API path of a resource under this client's repository. Every
// element is escaped, so a value cannot leave the repository's subtree.
func (g *github) repoPath(resource string, rest ...string) string {
	out := "/repos/" + url.PathEscape(g.repo.Owner) + "/" + url.PathEscape(g.repo.Name) + "/" + resource
	for _, element := range rest {
		out += "/" + escapeSegments(element)
	}
	return out
}

// response is an API answer. message is the forge's own error text, scrubbed and
// bounded.
type response struct {
	status  int
	message string
	body    []byte
}

func (r *response) ok() bool { return r.status >= 200 && r.status < 300 }

// call makes one API request. It fails on transport errors, redirects and
// oversized bodies; any other status is the caller's to interpret.
func (g *github) call(ctx context.Context, method, apiPath string, query url.Values, payload any) (*response, error) {
	endpoint := g.base.String() + apiPath
	if len(query) > 0 {
		endpoint += "?" + query.Encode()
	}
	var body io.Reader
	if payload != nil {
		encoded, err := json.Marshal(payload)
		if err != nil {
			return nil, g.failure("encode request", categoryInvalidRequest, err, nil)
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, body)
	if err != nil {
		return nil, g.failure("build request", categoryInvalidRequest, err, nil)
	}
	req.Header.Set("Authorization", "Bearer "+g.token)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "caesium")
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := g.http.Do(req)
	if err != nil {
		// A transport error can quote what the forge sent: an unparsable Location
		// header fails here, quoted whole, before CheckRedirect runs.
		return nil, g.failure("request", transportCategory(ctx, err), err, ctx.Err())
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 && resp.StatusCode < 400 {
		return nil, g.failure("request", fmt.Sprintf("redirect refused, status %d", resp.StatusCode),
			fmt.Errorf("refused a redirect (status %d)", resp.StatusCode), nil)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return nil, g.failure("read response", transportCategory(ctx, err), err, ctx.Err())
	}
	if len(raw) > maxResponseBytes {
		return nil, g.failure("read response", categoryTooLarge, errors.New("response exceeds the size limit"), nil)
	}
	out := &response{status: resp.StatusCode, body: raw}
	if !out.ok() {
		var problem struct {
			Message string `json:"message"`
			Errors  []struct {
				Message string `json:"message"`
			} `json:"errors"`
		}
		_ = json.Unmarshal(raw, &problem)
		parts := []string{problem.Message}
		for i := range problem.Errors {
			parts = append(parts, problem.Errors[i].Message)
		}
		out.message = truncate(g.scrub(strings.TrimSpace(strings.Join(parts, " "))), maxMessageBytes)
	}
	return out, nil
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return strings.ToValidUTF8(value[:limit], "")
}

// The categories a failure's Summary may carry. Each is fixed text: nothing the
// forge sent, and nothing a transport or parser error quoted from it.
const (
	categoryInvalidRequest = "invalid request"
	categoryTransport      = "transport error"
	categoryTimeout        = "timeout"
	categoryCancelled      = "cancelled"
	categoryTooLarge       = "response too large"
	categoryDecode         = "decode error"
	categoryNotEditable    = "not an editable file"
	categoryTooMany        = "too many pull requests"
)

// transportCategory names a failed exchange without its error text.
func transportCategory(ctx context.Context, err error) string {
	var timeout interface{ Timeout() bool }
	switch {
	case errors.Is(ctx.Err(), context.Canceled):
		return categoryCancelled
	case errors.Is(err, context.DeadlineExceeded), errors.Is(ctx.Err(), context.DeadlineExceeded),
		errors.As(err, &timeout) && timeout.Timeout():
		return categoryTimeout
	default:
		return categoryTransport
	}
}

// failure is the only way an error leaves this client: the text is scrubbed and
// the original chain dropped, so errors.Unwrap cannot reach an unscrubbed value.
// Only the sentinel in kind stays matchable. The text keeps err's detail, which
// can quote the forge, so Summary gets the fixed category instead.
func (g *github) failure(op, category string, err, kind error) error {
	return &clientError{
		text:    "forge: " + op + ": " + g.scrub(err.Error()),
		summary: "forge: " + op + ": " + category,
		kind:    kind,
	}
}

func (g *github) status(op string, resp *response, kind error) error {
	status := fmt.Sprintf("status %d", resp.status)
	text := status
	if resp.message != "" {
		text += ": " + resp.message
	}
	return &clientError{
		text:    "forge: " + op + ": " + g.scrub(text),
		summary: "forge: " + op + ": " + status,
		kind:    kind,
	}
}

type clientError struct {
	text    string
	summary string
	kind    error
}

func (e *clientError) Error() string { return e.text }
func (e *clientError) Unwrap() error { return e.kind }

// Summary is what failed, and its status or a fixed category, without anything
// the forge wrote. A forge's error text can quote what was sent to it (a
// pre-receive hook's output is the plain case), and a transport error can quote a
// header the forge sent, so neither reaches a reader who may not read the
// repository. Error() keeps that detail, so it never goes where Summary goes.
func Summary(err error) string {
	if failure, ok := errors.AsType[*clientError](err); ok {
		return failure.summary
	}
	return "forge: request failed"
}

// Redacted replaces the token wherever it appears in a forge's text.
const Redacted = "[REDACTED]"

func (g *github) scrub(text string) string {
	for _, form := range []string{
		g.token,
		url.QueryEscape(g.token),
		base64.StdEncoding.EncodeToString([]byte(g.token)),
	} {
		text = strings.ReplaceAll(text, form, Redacted)
	}
	return text
}

func (g *github) BranchHead(ctx context.Context, branch string) (string, error) {
	const op = "read branch"
	if err := ValidateBranch(branch); err != nil {
		return "", g.failure(op, categoryInvalidRequest, err, nil)
	}
	resp, err := g.call(ctx, http.MethodGet, g.repoPath("git/ref/heads", branch), nil, nil)
	if err != nil {
		return "", err
	}
	if resp.status == http.StatusNotFound {
		return "", g.status(op, resp, ErrNotFound)
	}
	if !resp.ok() {
		return "", g.status(op, resp, nil)
	}
	var ref struct {
		Ref    string `json:"ref"`
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(resp.body, &ref); err != nil {
		return "", g.failure(op, categoryDecode, errors.New("unexpected response"), nil)
	}
	if ref.Ref != "refs/heads/"+branch || !ValidCommit(ref.Object.SHA) {
		return "", g.failure(op, categoryDecode, errors.New("unexpected response"), nil)
	}
	return ref.Object.SHA, nil
}

func (g *github) GetFile(ctx context.Context, filePath, ref string) (*File, error) {
	const op = "read file"
	if err := ValidatePath(filePath); err != nil {
		return nil, g.failure(op, categoryInvalidRequest, err, nil)
	}
	resp, err := g.call(ctx, http.MethodGet, g.repoPath("contents", filePath), url.Values{"ref": {ref}}, nil)
	if err != nil {
		return nil, err
	}
	if resp.status == http.StatusNotFound {
		return nil, g.status(op, resp, ErrNotFound)
	}
	if !resp.ok() {
		return nil, g.status(op, resp, nil)
	}
	var file struct {
		Type     string `json:"type"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
		SHA      string `json:"sha"`
	}
	if err := json.Unmarshal(resp.body, &file); err != nil {
		// A directory answers with an array.
		return nil, g.failure(op, categoryNotEditable, errors.New("path is not a file"), ErrFileNotEditable)
	}
	if file.Type != "file" || file.Encoding != "base64" {
		return nil, g.failure(op, categoryNotEditable, errors.New("path is not an inline file"), ErrFileNotEditable)
	}
	content, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return nil, g.failure(op, categoryNotEditable, errors.New("undecodable content"), ErrFileNotEditable)
	}
	if file.SHA == "" {
		return nil, g.failure(op, categoryDecode, errors.New("unexpected response"), nil)
	}
	return &File{SHA: file.SHA, Content: content}, nil
}

func (g *github) CreateBranch(ctx context.Context, branch, sha string) (bool, error) {
	const op = "create branch"
	if err := ValidateBranch(branch); err != nil {
		return false, g.failure(op, categoryInvalidRequest, err, nil)
	}
	resp, err := g.call(ctx, http.MethodPost, g.repoPath("git/refs"), nil, map[string]string{
		"ref": "refs/heads/" + branch,
		"sha": sha,
	})
	if err != nil {
		return false, err
	}
	switch {
	case resp.ok():
		return true, nil
	case resp.status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(resp.message), "already exists"):
		return false, nil
	default:
		return false, g.status(op, resp, nil)
	}
}

func (g *github) PutFile(ctx context.Context, file PutFile) error {
	const op = "write file"
	if err := ValidatePath(file.Path); err != nil {
		return g.failure(op, categoryInvalidRequest, err, nil)
	}
	if err := ValidateBranch(file.Branch); err != nil {
		return g.failure(op, categoryInvalidRequest, err, nil)
	}
	resp, err := g.call(ctx, http.MethodPut, g.repoPath("contents", file.Path), nil, map[string]string{
		"message": file.Message,
		"content": base64.StdEncoding.EncodeToString(file.Content),
		"sha":     file.SHA,
		"branch":  file.Branch,
	})
	if err != nil {
		return err
	}
	switch {
	case resp.ok():
		return nil
	case resp.status == http.StatusConflict || resp.status == http.StatusUnprocessableEntity:
		return g.status(op, resp, ErrConflict)
	default:
		return g.status(op, resp, nil)
	}
}

// pullRequest is the API shape of a pull request.
type pullRequest struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
	State   string `json:"state"`
	// MergedAt is null unless the pull request was merged; the list endpoint
	// carries no `merged` flag.
	MergedAt *string `json:"merged_at"`
	Base     struct {
		Ref string `json:"ref"`
	} `json:"base"`
	Head struct {
		Ref  string `json:"ref"`
		SHA  string `json:"sha"`
		Repo *struct {
			FullName string `json:"full_name"`
		} `json:"repo"`
	} `json:"head"`
}

func (p *pullRequest) record() *PullRequest {
	return &PullRequest{
		Number: p.Number, URL: p.HTMLURL, State: p.State,
		Merged: p.MergedAt != nil && *p.MergedAt != "",
		Base:   p.Base.Ref, HeadSHA: p.Head.SHA,
	}
}

const (
	pullsPerPage = 100
	// maxPullPages bounds each pull request listing. A lookup that reaches it
	// without a decision fails rather than choosing from a partial list.
	maxPullPages = 10
)

// FindPullRequest reads the open pull requests from branch first: the forge
// allows one per head and base, so the list is short, and an open one into base
// is the answer. Without one it reads the closed ones and prefers a merged one,
// into base first. An open one into another base comes next, then a closed one,
// into base first. Every list is read to its end, page by page, before anything
// is chosen from it, except where nothing later could be preferred.
func (g *github) FindPullRequest(ctx context.Context, branch, base string) (*PullRequest, error) {
	const op = "list pull requests"
	if err := ValidateBranch(branch); err != nil {
		return nil, g.failure(op, categoryInvalidRequest, err, nil)
	}
	intoBase := func(p *PullRequest) bool { return p.Base == base }
	open, err := g.listPulls(ctx, op, branch, "open", intoBase)
	if err != nil {
		return nil, err
	}
	if found := firstOf(open, intoBase); found != nil {
		return found, nil
	}
	closed, err := g.listPulls(ctx, op, branch, "closed", func(p *PullRequest) bool { return p.Merged && intoBase(p) })
	if err != nil {
		return nil, err
	}
	for _, choice := range []*PullRequest{
		firstOf(closed, func(p *PullRequest) bool { return p.Merged && intoBase(p) }),
		firstOf(closed, func(p *PullRequest) bool { return p.Merged }),
		firstOf(open, func(*PullRequest) bool { return true }),
		firstOf(closed, intoBase),
		firstOf(closed, func(*PullRequest) bool { return true }),
	} {
		if choice != nil {
			return choice, nil
		}
	}
	return nil, nil
}

func firstOf(pulls []*PullRequest, match func(*PullRequest) bool) *PullRequest {
	for _, p := range pulls {
		if match(p) {
			return p
		}
	}
	return nil
}

// listPulls reads the pull requests from branch in state ("open" or "closed"),
// a page at a time, to the end of the list. done ends the read early at a pull
// request nothing later could be preferred to. A list longer than maxPullPages
// is ErrTooManyPullRequests: what lies past the bound could change the answer.
func (g *github) listPulls(ctx context.Context, op, branch, state string, done func(*PullRequest) bool) ([]*PullRequest, error) {
	var out []*PullRequest
	for page := 1; page <= maxPullPages; page++ {
		query := url.Values{
			"state": {state}, "head": {g.repo.Owner + ":" + branch},
			"per_page": {strconv.Itoa(pullsPerPage)}, "page": {strconv.Itoa(page)},
		}
		resp, err := g.call(ctx, http.MethodGet, g.repoPath("pulls"), query, nil)
		if err != nil {
			return nil, err
		}
		if !resp.ok() {
			return nil, g.status(op, resp, nil)
		}
		var pulls []pullRequest
		if err := json.Unmarshal(resp.body, &pulls); err != nil {
			return nil, g.failure(op, categoryDecode, errors.New("unexpected response"), nil)
		}
		for i := range pulls {
			// The API ignores a head filter it cannot parse and returns everything,
			// so the head and the state are checked here: another pull request must
			// not read as this one's.
			head := &pulls[i].Head
			if head.Ref != branch || (head.Repo != nil && !strings.EqualFold(head.Repo.FullName, g.repo.String())) {
				continue
			}
			found := pulls[i].record()
			if found.Open() != (state == "open") {
				continue
			}
			out = append(out, found)
			if done(found) {
				return out, nil
			}
		}
		if len(pulls) < pullsPerPage {
			return out, nil
		}
	}
	return nil, g.failure(op, categoryTooMany, errors.New("the pull request list exceeds the lookup bound"), ErrTooManyPullRequests)
}

func (g *github) OpenPullRequest(ctx context.Context, pr NewPullRequest) (*PullRequest, error) {
	const op = "open pull request"
	resp, err := g.call(ctx, http.MethodPost, g.repoPath("pulls"), nil, map[string]string{
		"title": pr.Title,
		"body":  pr.Body,
		"head":  pr.Head,
		"base":  pr.Base,
	})
	if err != nil {
		return nil, err
	}
	switch {
	case resp.ok():
	case resp.status == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(resp.message), "already exists"):
		return nil, g.status(op, resp, ErrConflict)
	default:
		return nil, g.status(op, resp, nil)
	}
	var opened pullRequest
	if err := json.Unmarshal(resp.body, &opened); err != nil || opened.Number == 0 {
		return nil, g.failure(op, categoryDecode, errors.New("unexpected response"), nil)
	}
	return opened.record(), nil
}
