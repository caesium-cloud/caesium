package forge

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

const testToken = "ghs_0123456789abcdefToken"

// newClient builds a real client against a test server, its repository host
// taken from the server so New's host check holds.
func newClient(t *testing.T, srv *httptest.Server) Client {
	t.Helper()
	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client, err := New(Config{
		Kind:       "github",
		APIBaseURL: srv.URL,
		Token:      testToken,
		Repo:       Repo{Host: parsed.Host, Owner: "acme", Name: "jobs"},
	})
	require.NoError(t, err)
	return client
}

func serve(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestParseRepoURL(t *testing.T) {
	want := Repo{Host: "github.com", Owner: "acme", Name: "jobs"}
	for _, raw := range []string{
		"https://github.com/acme/jobs",
		"https://github.com/acme/jobs.git",
		"https://GitHub.com/acme/jobs/",
		"https://user:secret@github.com/acme/jobs.git",
		"https://github.com:443/acme/jobs.git",
		"git@github.com:acme/jobs.git",
		"ssh://git@github.com/acme/jobs.git",
		"ssh://git@github.com:22/acme/jobs.git",
	} {
		got, err := ParseRepoURL(raw)
		require.NoError(t, err, raw)
		require.Equal(t, want, got, raw)
	}

	got, err := ParseRepoURL("http://127.0.0.1:8123/acme/jobs.git")
	require.NoError(t, err)
	require.Equal(t, "127.0.0.1:8123", got.Host)

	for _, raw := range []string{
		"",
		"/srv/git/jobs",
		"file:///srv/git/jobs",
		"https://github.com/acme",
		"https://github.com/acme/group/jobs",
		"https://github.com/../jobs",
		"https://github.com/acme/..",
		"https://github.com/acme/jo%2Fbs",
		"git@github.com:acme",
	} {
		_, err := ParseRepoURL(raw)
		require.Error(t, err, raw)
	}
}

func TestBranchForActionIsDeterministic(t *testing.T) {
	id := uuid.New()
	require.Equal(t, BranchForAction(id), BranchForAction(id))
	require.NotEqual(t, BranchForAction(id), BranchForAction(uuid.New()))
	require.NoError(t, ValidateBranch(BranchForAction(id)))
}

func TestValidateBranch(t *testing.T) {
	for _, name := range []string{"main", "release/1.2", "feature-x_y"} {
		require.NoError(t, ValidateBranch(name), name)
	}
	for _, name := range []string{
		"", "@", "-x", "/x", "x/", "x.", "a..b", "a//b", "a@{b", "a b", "a~b", "a^b",
		"a:b", "a?b", "a*b", "a[b", "a\\b", "a\nb", ".a", "a/.b", "a.lock", "a.lock/b",
	} {
		require.Error(t, ValidateBranch(name), "%q", name)
	}
}

func TestValidatePath(t *testing.T) {
	for _, value := range []string{"a.yaml", "jobs/a.job.yaml", ".github/x.yaml", "jobs/a b.yaml"} {
		require.NoError(t, ValidatePath(value), value)
	}
	for _, value := range []string{
		"", ".", "..", "/etc/passwd", "../a.yaml", "jobs/../a.yaml", "jobs/../../a.yaml",
		"jobs//a.yaml", "./a.yaml", "jobs/", "jobs\\a.yaml", "a\x00.yaml", "a\nb.yaml",
		".git/config", "sub/.git/hooks/x", "sub/.GIT/hooks/x",
	} {
		require.Error(t, ValidatePath(value), "%q", value)
	}
}

func TestNewRefusesAnAPIThatDoesNotServeTheRepository(t *testing.T) {
	config := Config{Kind: "github", Token: "t", APIBaseURL: "https://api.github.com"}

	config.Repo = Repo{Host: "gitlab.example.com", Owner: "a", Name: "b"}
	_, err := New(config)
	require.ErrorContains(t, err, "does not serve")

	config.Repo = Repo{Host: "github.com", Owner: "a", Name: "b"}
	_, err = New(config)
	require.NoError(t, err)

	config.APIBaseURL = "https://ghe.example.com/api/v3"
	_, err = New(config)
	require.ErrorContains(t, err, "does not serve")

	config.Repo.Host = "ghe.example.com"
	_, err = New(config)
	require.NoError(t, err)

	config.APIBaseURL = "http://ghe.example.com/api/v3"
	_, err = New(config)
	require.Error(t, err, "a token must not cross plain http")

	config.Kind = "gitlab"
	_, err = New(config)
	require.ErrorContains(t, err, "unsupported kind")
}

func TestRequestsCarryTheAPIShape(t *testing.T) {
	var seen *http.Request
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		seen = r
		_ = json.NewEncoder(w).Encode(map[string]any{
			"ref":    "refs/heads/release/1.2",
			"object": map[string]any{"sha": strings.Repeat("a", 40)},
		})
	})
	head, err := newClient(t, srv).BranchHead(context.Background(), "release/1.2")
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("a", 40), head)
	require.Equal(t, "/repos/acme/jobs/git/ref/heads/release/1.2", seen.URL.EscapedPath())
	require.Equal(t, "Bearer "+testToken, seen.Header.Get("Authorization"))
	require.Equal(t, "application/vnd.github+json", seen.Header.Get("Accept"))
	require.Equal(t, "2022-11-28", seen.Header.Get("X-GitHub-Api-Version"))
}

func TestEnterpriseBasePathIsKept(t *testing.T) {
	var escaped string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		escaped = r.URL.EscapedPath()
		w.WriteHeader(http.StatusNotFound)
	})
	parsed, err := url.Parse(srv.URL)
	require.NoError(t, err)
	client, err := New(Config{Kind: "github", APIBaseURL: srv.URL + "/api/v3/", Token: testToken,
		Repo: Repo{Host: parsed.Host, Owner: "acme", Name: "jobs"}})
	require.NoError(t, err)
	_, err = client.GetFile(context.Background(), "a.yaml", "main")
	require.ErrorIs(t, err, ErrNotFound)
	require.Equal(t, "/api/v3/repos/acme/jobs/contents/a.yaml", escaped)
}

// Every value in an API path is escaped by segment, so a space, '#' or '?' in a
// manifest path cannot change the resource or the query.
func TestFilePathIsEscapedBySegment(t *testing.T) {
	var escaped, ref string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		escaped, ref = r.URL.EscapedPath(), r.URL.Query().Get("ref")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"type": "file", "encoding": "base64", "sha": "blob",
			"content": base64.StdEncoding.EncodeToString([]byte("x\n")),
		})
	})
	file, err := newClient(t, srv).GetFile(context.Background(), "jobs/a b#c?d.yaml", "main&evil=1")
	require.NoError(t, err)
	require.Equal(t, "x\n", string(file.Content))
	require.Equal(t, "/repos/acme/jobs/contents/jobs/a%20b%23c%3Fd.yaml", escaped)
	require.Equal(t, "main&evil=1", ref)
}

func TestClientRefusesAnInvalidPathBeforeAnyRequest(t *testing.T) {
	var hits atomic.Int32
	srv := serve(t, func(http.ResponseWriter, *http.Request) { hits.Add(1) })
	client := newClient(t, srv)
	ctx := context.Background()

	_, err := client.GetFile(ctx, "../secrets.yaml", "main")
	require.Error(t, err)
	require.Error(t, client.PutFile(ctx, PutFile{Path: "a/../../b.yaml", Branch: "x"}))
	require.Error(t, client.PutFile(ctx, PutFile{Path: "a.yaml", Branch: "x..y"}))
	_, err = client.CreateBranch(ctx, "x y", "sha")
	require.Error(t, err)
	require.Zero(t, hits.Load())
}

// The token stays with the configured API: a redirect is an error, and the
// redirect target receives nothing at all.
func TestRedirectIsNeverFollowed(t *testing.T) {
	var foreign atomic.Int32
	other := serve(t, func(http.ResponseWriter, *http.Request) { foreign.Add(1) })
	for _, status := range []int{http.StatusMovedPermanently, http.StatusFound, http.StatusTemporaryRedirect} {
		srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
			http.Redirect(w, r, other.URL+"/steal", status)
		})
		_, err := newClient(t, srv).BranchHead(context.Background(), "main")
		require.ErrorContains(t, err, "redirect")
	}
	require.Zero(t, foreign.Load())
}

func TestErrorsNeverCarryTheToken(t *testing.T) {
	encoded := base64.StdEncoding.EncodeToString([]byte(testToken))
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"message": "bad credential " + testToken + " or " + encoded + " or " + url.QueryEscape(testToken),
			"errors":  []map[string]string{{"message": "again " + testToken}},
		})
	})
	_, err := newClient(t, srv).BranchHead(context.Background(), "main")
	require.Error(t, err)
	require.NotContains(t, err.Error(), testToken)
	require.NotContains(t, err.Error(), encoded)
	require.Contains(t, err.Error(), "status 500")
	require.Contains(t, err.Error(), Redacted)
}

func TestErrorChainHoldsNoUnscrubbedCause(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"message":"`+testToken+`"}`)
	})
	_, err := newClient(t, srv).BranchHead(context.Background(), "main")
	for ; err != nil; err = errors.Unwrap(err) {
		require.NotContains(t, err.Error(), testToken)
	}
}

func TestResponseSizeIsBounded(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(make([]byte, maxResponseBytes+1))
	})
	_, err := newClient(t, srv).BranchHead(context.Background(), "main")
	require.ErrorContains(t, err, "size limit")
}

func TestContextCancellationStopsARequest(t *testing.T) {
	release := make(chan struct{})
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	defer close(release)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := newClient(t, srv).BranchHead(ctx, "main")
	require.ErrorIs(t, err, context.Canceled)
}

func TestGetFileRefusesWhatTheContentsAPICannotRoundTrip(t *testing.T) {
	cases := map[string]any{
		"directory": []map[string]any{{"type": "file", "name": "a.yaml"}},
		"too large": map[string]any{"type": "file", "encoding": "none", "content": "", "sha": "b"},
		"submodule": map[string]any{"type": "submodule", "sha": "b"},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode(body)
			})
			_, err := newClient(t, srv).GetFile(context.Background(), "a.yaml", "main")
			require.ErrorIs(t, err, ErrFileNotEditable)
		})
	}
}

func TestCreateBranchTreatsAnExistingRefAsALookup(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"Reference already exists"}`)
	})
	created, err := newClient(t, srv).CreateBranch(context.Background(), "caesium/patch-1", "sha")
	require.NoError(t, err)
	require.False(t, created)
}

func TestCreateBranchReportsAnyOtherRefusal(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"Object does not exist"}`)
	})
	_, err := newClient(t, srv).CreateBranch(context.Background(), "caesium/patch-1", "sha")
	require.ErrorContains(t, err, "Object does not exist")
}

func TestPutFileMapsAStaleBlobToConflict(t *testing.T) {
	for _, status := range []int{http.StatusConflict, http.StatusUnprocessableEntity} {
		srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
			var body map[string]string
			require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
			require.Equal(t, "blob", body["sha"])
			require.Equal(t, "caesium/patch-1", body["branch"])
			require.Equal(t, base64.StdEncoding.EncodeToString([]byte("new")), body["content"])
			w.WriteHeader(status)
		})
		err := newClient(t, srv).PutFile(context.Background(), PutFile{
			Path: "a.yaml", Branch: "caesium/patch-1", Message: "m", Content: []byte("new"), SHA: "blob",
		})
		require.ErrorIs(t, err, ErrConflict)
	}
}

// The list endpoint ignores a head filter it cannot parse and returns every pull
// request, so only a matching head may read as this action's pull request.
func TestFindPullRequestChecksTheHead(t *testing.T) {
	head := func(ref, repo string) map[string]any {
		return map[string]any{"ref": ref, "repo": map[string]any{"full_name": repo}}
	}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, "acme:caesium/patch-1", r.URL.Query().Get("head"))
		require.Contains(t, []string{"open", "closed"}, r.URL.Query().Get("state"))
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"number": 1, "html_url": "https://x/1", "state": "open", "head": head("other", "acme/jobs")},
			{"number": 2, "html_url": "https://x/2", "state": "open", "head": head("caesium/patch-1", "fork/jobs")},
			{"number": 3, "html_url": "https://x/3", "state": "closed", "head": head("caesium/patch-1", "acme/jobs")},
		})
	})
	found, err := newClient(t, srv).FindPullRequest(context.Background(), "caesium/patch-1", "main")
	require.NoError(t, err)
	require.Equal(t, &PullRequest{Number: 3, URL: "https://x/3", State: "closed"}, found)
}

// listedPull is a pull request on the branch caesium/patch-1 as the list endpoint
// returns it.
func listedPull(number int, state string, mergedAt any, base string) map[string]any {
	return map[string]any{
		"number": number, "html_url": "https://x/" + strconv.Itoa(number), "state": state, "merged_at": mergedAt,
		"base": map[string]any{"ref": base},
		"head": map[string]any{"ref": "caesium/patch-1", "sha": strings.Repeat("a", 40), "repo": map[string]any{"full_name": "acme/jobs"}},
	}
}

// pagingForge serves pulls as the list endpoint does, per_page at a time. With
// honorState false it ignores the state filter, as it does a head filter it
// cannot parse, so the client's own state check is what holds. lists counts the
// requests by state.
func pagingForge(t *testing.T, pulls []map[string]any, honorState bool) (Client, map[string]int) {
	t.Helper()
	var mu sync.Mutex
	lists := map[string]int{}
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		query := r.URL.Query()
		mu.Lock()
		lists[query.Get("state")]++
		mu.Unlock()
		var matched []map[string]any
		for _, pull := range pulls {
			if !honorState || (pull["state"] == "open") == (query.Get("state") == "open") {
				matched = append(matched, pull)
			}
		}
		perPage, _ := strconv.Atoi(query.Get("per_page"))
		page, _ := strconv.Atoi(query.Get("page"))
		require.Equal(t, 100, perPage)
		start := min((page-1)*perPage, len(matched))
		out := matched[start:min(start+perPage, len(matched))]
		if out == nil {
			out = []map[string]any{}
		}
		_ = json.NewEncoder(w).Encode(out)
	})
	return newClient(t, srv), lists
}

func closedPulls(from, count int, base string) []map[string]any {
	out := make([]map[string]any, 0, count)
	for i := range count {
		out = append(out, listedPull(from+i, "closed", nil, base))
	}
	return out
}

// Several pull requests can share the branch: an open one into the base wins,
// then a merged one into it, then any merged one, then an open one into another
// base, then a closed one. merged_at tells a merged pull request from a rejected
// one; the forge reports both as closed.
func TestFindPullRequestPrefersOpenThenMerged(t *testing.T) {
	const at = "2026-11-01T12:00:00Z"
	rejected, merged, open := listedPull(1, "closed", nil, "main"), listedPull(2, "closed", at, "main"), listedPull(3, "open", nil, "main")
	openElsewhere, mergedElsewhere := listedPull(4, "open", nil, "release"), listedPull(5, "closed", at, "release")
	for name, tc := range map[string]struct {
		listed []map[string]any
		want   int
	}{
		"open over merged and rejected":        {listed: []map[string]any{rejected, merged, open}, want: 3},
		"open into the base over another base": {listed: []map[string]any{openElsewhere, open}, want: 3},
		"merged over rejected":                 {listed: []map[string]any{rejected, merged}, want: 2},
		"merged into the base over elsewhere":  {listed: []map[string]any{mergedElsewhere, merged}, want: 2},
		"merged elsewhere over open elsewhere": {listed: []map[string]any{openElsewhere, mergedElsewhere}, want: 5},
		"open elsewhere over rejected":         {listed: []map[string]any{rejected, openElsewhere}, want: 4},
		"rejected alone":                       {listed: []map[string]any{rejected}, want: 1},
	} {
		t.Run(name, func(t *testing.T) {
			for _, honorState := range []bool{true, false} {
				client, _ := pagingForge(t, tc.listed, honorState)
				found, err := client.FindPullRequest(context.Background(), "caesium/patch-1", "main")
				require.NoError(t, err)
				require.NotNil(t, found)
				require.Equal(t, tc.want, found.Number, "honorState=%v", honorState)
			}
		})
	}
	client, _ := pagingForge(t, []map[string]any{merged}, true)
	found, err := client.FindPullRequest(context.Background(), "caesium/patch-1", "main")
	require.NoError(t, err)
	require.Equal(t, &PullRequest{Number: 2, URL: "https://x/2", State: "closed", Merged: true, Base: "main", HeadSHA: strings.Repeat("a", 40)}, found)
}

// Every page is read before a choice: the list is newest first, so an open pull
// request or a merged one can sit behind a page of closed ones.
func TestFindPullRequestReadsEveryPage(t *testing.T) {
	const at = "2026-11-01T12:00:00Z"
	for name, tc := range map[string]struct {
		listed     []map[string]any
		honorState bool
		want       int
	}{
		"open behind a page of closed ones": {
			listed: append(closedPulls(1, 150, "release"), listedPull(900, "open", nil, "main")), want: 900},
		"merged behind a page of rejected ones": {
			listed: append(closedPulls(1, 150, "main"), listedPull(900, "closed", at, "main")), honorState: true, want: 900},
		"merged into the base behind one elsewhere": {
			listed: append(append([]map[string]any{listedPull(800, "closed", at, "release")}, closedPulls(1, 150, "main")...),
				listedPull(900, "closed", at, "main")), honorState: true, want: 900},
	} {
		t.Run(name, func(t *testing.T) {
			client, _ := pagingForge(t, tc.listed, tc.honorState)
			found, err := client.FindPullRequest(context.Background(), "caesium/patch-1", "main")
			require.NoError(t, err)
			require.NotNil(t, found)
			require.Equal(t, tc.want, found.Number)
		})
	}
}

// A list longer than the bound is never chosen from: what lies past it could
// change the answer. A pull request nothing could beat ends the read early.
func TestFindPullRequestFailsClosedAtTheBound(t *testing.T) {
	const at = "2026-11-01T12:00:00Z"
	beyond := maxPullPages*pullsPerPage + 1

	client, lists := pagingForge(t, closedPulls(1, beyond, "main"), true)
	_, err := client.FindPullRequest(context.Background(), "caesium/patch-1", "main")
	require.ErrorIs(t, err, ErrTooManyPullRequests)
	require.Equal(t, "forge: list pull requests: too many pull requests", Summary(err))
	require.Equal(t, maxPullPages, lists["closed"], "it reads to the bound and no further")

	open := make([]map[string]any, 0, beyond)
	for i := range beyond {
		open = append(open, listedPull(i+1, "open", nil, "release"))
	}
	client, _ = pagingForge(t, open, true)
	_, err = client.FindPullRequest(context.Background(), "caesium/patch-1", "main")
	require.ErrorIs(t, err, ErrTooManyPullRequests, "the open list is bounded too")

	client, lists = pagingForge(t, append([]map[string]any{listedPull(1, "closed", at, "main")}, closedPulls(2, beyond, "main")...), true)
	found, err := client.FindPullRequest(context.Background(), "caesium/patch-1", "main")
	require.NoError(t, err)
	require.Equal(t, 1, found.Number)
	require.Equal(t, 1, lists["closed"], "a merged pull request into the base ends the read")
}

func TestFindPullRequestFindsNothing(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) { _, _ = io.WriteString(w, `[]`) })
	found, err := newClient(t, srv).FindPullRequest(context.Background(), "caesium/patch-1", "main")
	require.NoError(t, err)
	require.Nil(t, found)
}

func TestOpenPullRequest(t *testing.T) {
	var body map[string]string
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		require.Equal(t, http.MethodPost, r.Method)
		require.Equal(t, "/repos/acme/jobs/pulls", r.URL.Path)
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		w.WriteHeader(http.StatusCreated)
		_, _ = io.WriteString(w, `{"number":7,"html_url":"https://x/7","state":"open"}`)
	})
	opened, err := newClient(t, srv).OpenPullRequest(context.Background(), NewPullRequest{
		Title: "t", Body: "b", Head: "caesium/patch-1", Base: "main",
	})
	require.NoError(t, err)
	require.Equal(t, 7, opened.Number)
	require.Equal(t, map[string]string{"title": "t", "body": "b", "head": "caesium/patch-1", "base": "main"}, body)
}

func TestOpenPullRequestReportsAnExistingOneAsConflict(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"Validation Failed","errors":[{"message":"A pull request already exists for acme:x."}]}`)
	})
	_, err := newClient(t, srv).OpenPullRequest(context.Background(), NewPullRequest{})
	require.ErrorIs(t, err, ErrConflict)
}

func TestForgeErrorMessageIsBounded(t *testing.T) {
	srv := serve(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": strings.Repeat("x", 4096)})
	})
	_, err := newClient(t, srv).BranchHead(context.Background(), "main")
	require.Less(t, len(err.Error()), 300)
}

// Summary keeps which call failed and its status, and nothing the forge wrote.
func TestSummaryHoldsNothingTheForgeWrote(t *testing.T) {
	client := newClient(t, serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
		_, _ = io.WriteString(w, `{"message":"hook declined: quoted-from-the-file"}`)
	}))
	_, err := client.BranchHead(context.Background(), "main")
	require.ErrorContains(t, err, "quoted-from-the-file", "the error itself keeps the detail")
	require.Equal(t, "forge: read branch: status 422", Summary(err))
	require.Equal(t, "forge: request failed", Summary(errors.New("quoted-from-the-file")))
}

// A transport error can quote the forge as well: Go's client fails on a Location
// header it cannot parse before CheckRedirect runs, and quotes the whole value.
// Summary carries the call and a fixed category.
func TestSummaryOfATransportErrorQuotesNothing(t *testing.T) {
	const marker = "finance-webhook-secret"
	client := newClient(t, serve(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header()["Location"] = []string{"/" + marker + "%zz"}
		w.WriteHeader(http.StatusFound)
	}))
	_, err := client.BranchHead(context.Background(), "main")
	require.ErrorContains(t, err, marker, "the transport error quotes the header")
	require.Equal(t, "forge: request: transport error", Summary(err))
}

// Every failure that is not a status has a fixed summary.
func TestEveryFailureSummaryIsFixedText(t *testing.T) {
	const marker = "quoted-by-the-forge"
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	cases := map[string]struct {
		handler http.HandlerFunc
		ctx     func() (context.Context, context.CancelFunc)
		call    func(Client, context.Context) error
		summary string
	}{
		"redirect": {
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.Redirect(w, r, "https://elsewhere.example/"+marker, http.StatusTemporaryRedirect)
			},
			summary: "forge: request: redirect refused, status 307",
		},
		"timeout": {
			handler: func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-release:
				case <-r.Context().Done():
				}
			},
			ctx: func() (context.Context, context.CancelFunc) {
				return context.WithTimeout(context.Background(), 50*time.Millisecond)
			},
			summary: "forge: request: timeout",
		},
		"cancelled": {
			handler: func(http.ResponseWriter, *http.Request) {},
			ctx: func() (context.Context, context.CancelFunc) {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				return ctx, cancel
			},
			summary: "forge: request: cancelled",
		},
		"too large": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(marker + strings.Repeat(" ", maxResponseBytes)))
			},
			summary: "forge: read response: response too large",
		},
		"decode": {
			handler: func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"ref":"`+marker+`"}`) },
			summary: "forge: read branch: decode error",
		},
		"not editable": {
			handler: func(w http.ResponseWriter, _ *http.Request) {
				_, _ = io.WriteString(w, `{"type":"symlink","target":"`+marker+`","sha":"b"}`)
			},
			call: func(c Client, ctx context.Context) error {
				_, err := c.GetFile(ctx, "a.yaml", "main")
				return err
			},
			summary: "forge: read file: not an editable file",
		},
		"invalid request": {
			handler: func(http.ResponseWriter, *http.Request) {},
			call: func(c Client, ctx context.Context) error {
				_, err := c.GetFile(ctx, "../"+marker, "main")
				return err
			},
			summary: "forge: read file: invalid request",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.Background(), context.CancelFunc(func() {})
			if tc.ctx != nil {
				ctx, cancel = tc.ctx()
			}
			defer cancel()
			call := tc.call
			if call == nil {
				call = func(c Client, ctx context.Context) error {
					_, err := c.BranchHead(ctx, "main")
					return err
				}
			}
			err := call(newClient(t, serve(t, tc.handler)), ctx)
			require.Error(t, err)
			require.Equal(t, tc.summary, Summary(err))
		})
	}
}
