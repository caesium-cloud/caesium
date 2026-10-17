package policy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/metrics"
	metrictestutil "github.com/caesium-cloud/caesium/internal/metrics/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/stretchr/testify/require"
)

const (
	loaderHeader  = "apiVersion: v1\nkind: AccessPolicy\n"
	loaderPolicyA = `apiVersion: v1
kind: AccessPolicy
namespaces:
  marketing:
    secrets: {allow: ["env/MARKETING_*"]}
    quotas: {maxConcurrentRuns: 3}
bindings:
  - subjects: {groups: [viewers]}
    role: viewer
    namespaces: ["*"]
  - subjects: {groups: [marketing-eng]}
    role: operator
    namespaces: [marketing]
`
	loaderPolicyB = `apiVersion: v1
kind: AccessPolicy
bindings:
  - subjects: {groups: [admins]}
    role: admin
    namespaces: ["*"]
`
	loaderPolicyInvalid = `apiVersion: v1
kind: AccessPolicy
bindings:
  - subjects: {groups: [viewers]}
    role: superuser
    namespaces: ["*"]
`
	loaderPolicyInvalid2 = `apiVersion: v1
kind: AccessPolicy
bindings:
  - subjects: {groups: [viewers]}
    role: viewer
    namespaces: [undeclared]
`
)

func sha(body string) string {
	sum := sha256.Sum256([]byte(body))
	return hex.EncodeToString(sum[:])
}

// writePolicy replaces path atomically, as a ConfigMap swap or the
// integration scenario does, so a concurrent poll never reads half a file.
func writePolicy(t *testing.T, path, body string) {
	t.Helper()
	tmp, err := os.CreateTemp(filepath.Dir(path), ".policy-*.tmp")
	require.NoError(t, err)
	_, err = tmp.WriteString(body)
	require.NoError(t, err)
	require.NoError(t, tmp.Close())
	require.NoError(t, os.Rename(tmp.Name(), path))
}

type eventLog struct {
	mu     sync.Mutex
	events []ReloadEvent
}

func (l *eventLog) record(event ReloadEvent) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, event)
}

func (l *eventLog) all() []ReloadEvent {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]ReloadEvent(nil), l.events...)
}

func loadTestHolder(t *testing.T, body string, opts HolderOptions) (*Holder, string, *eventLog) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "policy.yaml")
	writePolicy(t, path, body)
	opts.Path = path
	h, err := LoadFile(t.Context(), opts)
	require.NoError(t, err)
	events := &eventLog{}
	h.SetObserver(events.record)
	return h, path, events
}

func failures(t *testing.T, trigger string) float64 {
	t.Helper()
	return metrictestutil.CounterValue(t, metrics.AuthPolicyReloadFailuresTotal, trigger)
}

func TestLoadFileServesValidatedRevision(t *testing.T) {
	t.Parallel()
	h, path, _ := loadTestHolder(t, loaderPolicyA, HolderOptions{})

	rev := h.Current()
	require.NotNil(t, rev)
	require.Equal(t, SourceFile, rev.Source)
	require.Equal(t, sha(loaderPolicyA), rev.Hash)
	require.False(t, rev.LoadedAt.IsZero())
	require.True(t, h.FileBacked())
	require.Equal(t, path, h.Path())
	require.Nil(t, h.LastReload(), "the startup load is not a reload attempt")

	require.Equal(t, Grants{"*": models.RoleViewer}, h.Resolve([]string{"viewers"}, ""))
	require.Equal(t, Grants{"marketing": models.RoleOperator}, h.Resolve([]string{"marketing-eng"}, ""))
	require.Empty(t, h.Resolve([]string{"nobody"}, ""))
	require.NotNil(t, h.Resolve(nil, ""))

	// Fresh map per call: a caller mutating its grants cannot leak.
	grants := h.Resolve([]string{"viewers"}, "")
	grants["*"] = models.RoleAdmin
	require.Equal(t, Grants{"*": models.RoleViewer}, h.Resolve([]string{"viewers"}, ""))
}

func TestHolderNamespaceAccessor(t *testing.T) {
	t.Parallel()
	h, _, _ := loadTestHolder(t, loaderPolicyA, HolderOptions{})

	ns, declared := h.Namespace("marketing")
	require.True(t, declared)
	require.Equal(t, []string{"env/MARKETING_*"}, ns.Secrets.Allow)
	require.Equal(t, 3, ns.Quotas.MaxConcurrentRuns)

	_, declared = h.Namespace(DefaultNamespace)
	require.True(t, declared, "default is implicitly declared")
	_, declared = h.Namespace("finance")
	require.False(t, declared)
	_, declared = h.Namespace("*")
	require.False(t, declared, "* is a grant scope, never a namespace")

	// The accessor returns a copy; the served revision stays immutable.
	ns.Secrets.Allow[0] = "env/*"
	ns.Quotas.MaxConcurrentRuns = 99
	again, _ := h.Namespace("marketing")
	require.Equal(t, []string{"env/MARKETING_*"}, again.Secrets.Allow)
	require.Equal(t, 3, again.Quotas.MaxConcurrentRuns)

	var nilHolder *Holder
	_, declared = nilHolder.Namespace(DefaultNamespace)
	require.True(t, declared)
	_, declared = nilHolder.Namespace("marketing")
	require.False(t, declared)
}

func TestLoadFileErrorsNameTheFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	invalid := filepath.Join(dir, "invalid.yaml")
	require.NoError(t, os.WriteFile(invalid, []byte(loaderPolicyInvalid), 0o600))
	oversize := filepath.Join(dir, "oversize.yaml")
	require.NoError(t, os.WriteFile(oversize, []byte(loaderPolicyA+"#"+strings.Repeat("x", MaxFileBytes)+"\n"), 0o600))
	exact := filepath.Join(dir, "exact.yaml")
	padded := loaderPolicyA + "#" + strings.Repeat("x", MaxFileBytes-len(loaderPolicyA)-2) + "\n"
	require.Len(t, padded, MaxFileBytes)
	require.NoError(t, os.WriteFile(exact, []byte(padded), 0o600))

	for _, tc := range []struct {
		path, want string
	}{
		{filepath.Join(dir, "absent.yaml"), "no such file"},
		{invalid, `invalid role "superuser"`},
		{oversize, "exceeds 1048576 bytes"},
		{dir, "not a regular file"},
	} {
		h, err := LoadFile(t.Context(), HolderOptions{Path: tc.path})
		require.Error(t, err, tc.path)
		require.Nil(t, h)
		require.Contains(t, err.Error(), "access policy file "+tc.path)
		require.Contains(t, err.Error(), tc.want)
	}

	// A FIFO is refused without opening it (an open would block until a
	// writer appeared, holding the reload lock).
	fifo := filepath.Join(dir, "fifo.yaml")
	require.NoError(t, syscall.Mkfifo(fifo, 0o600))
	_, err := LoadFile(t.Context(), HolderOptions{Path: fifo})
	require.ErrorContains(t, err, "not a regular file")

	h, err := LoadFile(t.Context(), HolderOptions{Path: exact})
	require.NoError(t, err, "a file of exactly MaxFileBytes is accepted")
	require.Equal(t, sha(padded), h.Current().Hash)

	_, err = LoadFile(t.Context(), HolderOptions{Path: " "})
	require.Error(t, err)
	_, err = LoadFile(t.Context(), HolderOptions{Path: exact, DefaultRole: "root"})
	require.ErrorContains(t, err, `invalid default role "root"`)
}

func TestReloadSwapsValidRevisionAndReportsIt(t *testing.T) {
	t.Parallel()
	h, path, events := loadTestHolder(t, loaderPolicyA, HolderOptions{})
	before := h.Current()

	writePolicy(t, path, loaderPolicyB)
	result, err := h.Reload(t.Context(), ReloadRequest{Actor: "csk_live_admin", SourceIP: "198.51.100.8"})
	require.NoError(t, err)
	require.True(t, result.Changed)
	require.Same(t, before, result.Previous)
	require.Same(t, result.Current, h.Current())
	require.Equal(t, sha(loaderPolicyB), h.Current().Hash)

	// The next resolution uses the new revision.
	require.Empty(t, h.Resolve([]string{"viewers"}, ""))
	require.Equal(t, Grants{"*": models.RoleAdmin}, h.Resolve([]string{"admins"}, ""))

	require.Equal(t, []ReloadEvent{{
		At: events.all()[0].At, Trigger: TriggerAPI, Actor: "csk_live_admin", SourceIP: "198.51.100.8",
		Path: path, OK: true, Changed: true,
		PreviousRevision: sha(loaderPolicyA), Revision: sha(loaderPolicyB),
	}}, events.all())

	last := h.LastReload()
	require.NotNil(t, last)
	require.True(t, last.OK)
	require.True(t, last.Changed)
	require.Equal(t, TriggerAPI, last.Trigger)
	require.Equal(t, sha(loaderPolicyB), last.Revision)
}

// Not parallel: asserts a delta on the global failure counter.
func TestInvalidReloadKeepsLastGoodRevision(t *testing.T) {
	h, path, events := loadTestHolder(t, loaderPolicyA, HolderOptions{})
	good := h.Current()
	before := failures(t, TriggerAPI)

	writePolicy(t, path, loaderPolicyInvalid)
	result, err := h.Reload(t.Context(), ReloadRequest{Actor: "csk_live_admin"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "access policy file "+path)
	require.Contains(t, err.Error(), `invalid role "superuser"`)
	require.False(t, result.Changed)
	require.Same(t, good, result.Current, "the failure reports the retained revision")
	require.Same(t, good, h.Current(), "an invalid revision never replaces the last good one")
	require.Equal(t, Grants{"*": models.RoleViewer}, h.Resolve([]string{"viewers"}, ""))
	require.Equal(t, before+1, failures(t, TriggerAPI))

	got := events.all()
	require.Len(t, got, 1)
	require.False(t, got[0].OK)
	require.Equal(t, sha(loaderPolicyA), got[0].PreviousRevision)
	require.Equal(t, sha(loaderPolicyInvalid), got[0].Revision)
	require.Equal(t, err.Error(), got[0].Error)

	last := h.LastReload()
	require.False(t, last.OK)
	require.Equal(t, err.Error(), last.Error)
	require.Equal(t, sha(loaderPolicyInvalid), last.Revision)

	// The API trigger reports every attempt, the same bad file included.
	_, err = h.Reload(t.Context(), ReloadRequest{Actor: "csk_live_admin"})
	require.Error(t, err)
	require.Len(t, events.all(), 2)
	require.Equal(t, before+2, failures(t, TriggerAPI))
}

func TestMissingFileAtReloadIsAFailureNotAnEmptyPolicy(t *testing.T) {
	h, path, events := loadTestHolder(t, loaderPolicyA, HolderOptions{})
	good := h.Current()
	require.NoError(t, os.Remove(path))

	_, err := h.Reload(t.Context(), ReloadRequest{Actor: "a"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "no such file")
	require.Same(t, good, h.Current())
	require.Equal(t, Grants{"*": models.RoleViewer}, h.Resolve([]string{"viewers"}, ""))
	got := events.all()
	require.Len(t, got, 1)
	require.False(t, got[0].OK)
	require.Empty(t, got[0].Revision, "an unreadable file has no hash")

	// The poller reports the same unreadable file once.
	h.poll(t.Context())
	h.poll(t.Context())
	require.Len(t, events.all(), 1)
	require.Same(t, good, h.Current())
}

func TestUnchangedAPIReloadIsReportedPollIsSilent(t *testing.T) {
	t.Parallel()
	h, _, events := loadTestHolder(t, loaderPolicyA, HolderOptions{})
	good := h.Current()

	h.poll(t.Context())
	h.poll(t.Context())
	require.Empty(t, events.all(), "a poll that finds the loaded hash writes nothing")
	require.Nil(t, h.LastReload())

	result, err := h.Reload(t.Context(), ReloadRequest{Actor: "csk_live_admin"})
	require.NoError(t, err)
	require.False(t, result.Changed)
	require.Same(t, good, h.Current())
	got := events.all()
	require.Len(t, got, 1, "an API reload always writes one row")
	require.True(t, got[0].OK)
	require.False(t, got[0].Changed)
	require.Equal(t, got[0].PreviousRevision, got[0].Revision)
}

func TestPollReportsEachBadFileOnce(t *testing.T) {
	h, path, events := loadTestHolder(t, loaderPolicyA, HolderOptions{})
	good := h.Current()
	before := failures(t, TriggerPoll)

	writePolicy(t, path, loaderPolicyInvalid)
	h.poll(t.Context())
	require.Empty(t, events.all(), "a change is acted on only when the next poll reads the same bytes")
	require.Equal(t, before, failures(t, TriggerPoll))
	for range 3 {
		h.poll(t.Context())
	}
	require.Len(t, events.all(), 1, "one failure per distinct bad file, not one per interval")
	require.Equal(t, before+1, failures(t, TriggerPoll))
	require.Equal(t, PollActor, events.all()[0].Actor)
	require.Equal(t, TriggerPoll, events.all()[0].Trigger)
	require.Same(t, good, h.Current())

	writePolicy(t, path, loaderPolicyInvalid2)
	h.poll(t.Context())
	h.poll(t.Context())
	require.Len(t, events.all(), 2, "a different bad file is a new failure")
	require.Contains(t, events.all()[1].Error, `undeclared namespace "undeclared"`)
	require.Equal(t, before+2, failures(t, TriggerPoll))

	// Reverting to the served file clears the failure without an audit row.
	writePolicy(t, path, loaderPolicyA)
	h.poll(t.Context())
	require.Len(t, events.all(), 2)
	last := h.LastReload()
	require.True(t, last.OK)
	require.False(t, last.Changed)
	require.Equal(t, good.Hash, last.Revision)

	// The first bad file is reported again once it reappears.
	writePolicy(t, path, loaderPolicyInvalid)
	h.poll(t.Context())
	h.poll(t.Context())
	require.Len(t, events.all(), 3)
	require.Equal(t, before+3, failures(t, TriggerPoll))
}

func TestPollPicksUpAValidChange(t *testing.T) {
	t.Parallel()
	h, path, events := loadTestHolder(t, loaderPolicyA, HolderOptions{})

	writePolicy(t, path, loaderPolicyB)
	h.poll(t.Context())
	require.Equal(t, sha(loaderPolicyA), h.Current().Hash, "the first sighting only marks the change pending")
	require.Empty(t, events.all())
	h.poll(t.Context())
	require.Equal(t, sha(loaderPolicyB), h.Current().Hash)
	got := events.all()
	require.Len(t, got, 1)
	require.Equal(t, ReloadEvent{
		At: got[0].At, Trigger: TriggerPoll, Actor: PollActor, Path: path, OK: true, Changed: true,
		PreviousRevision: sha(loaderPolicyA), Revision: sha(loaderPolicyB),
	}, got[0])

	h.poll(t.Context())
	require.Len(t, events.all(), 1)
}

// loaderPolicyAFull extends loaderPolicyA with a trailing admin binding, so a
// cut-off prefix of it (loaderPolicyA itself) is still a valid document.
const loaderPolicyAFull = loaderPolicyA + `  - subjects: {groups: [admins]}
    role: admin
    namespaces: ["*"]
`

func appendToFile(t *testing.T, path, text string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	require.NoError(t, err)
	_, err = f.WriteString(text)
	require.NoError(t, err)
	require.NoError(t, f.Close())
}

// TestPollNeverActsOnAnInPlaceRewriteMidWrite is the in-place-writer
// regression: `cp`, `cat >` or an editor rewrites the file non-atomically, so
// one poll can see it empty or cut off. The poller acts only when two
// consecutive polls read the same bytes, so neither a spurious failure nor a
// truncated (but valid) document is published.
func TestPollNeverActsOnAnInPlaceRewriteMidWrite(t *testing.T) {
	h, path, events := loadTestHolder(t, loaderPolicyB, HolderOptions{})
	before := failures(t, TriggerPoll)

	// The writer has truncated the file: an empty read is not a failure yet.
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	h.poll(t.Context())
	// It has written a valid prefix (the trailing admin binding is missing).
	require.NoError(t, os.WriteFile(path, []byte(loaderPolicyA), 0o600))
	h.poll(t.Context())
	// It finishes.
	appendToFile(t, path, loaderPolicyAFull[len(loaderPolicyA):])
	h.poll(t.Context())
	require.Empty(t, events.all(), "no intermediate state is published or reported")
	require.Equal(t, before, failures(t, TriggerPoll), "the empty read raised no failure")
	require.Equal(t, sha(loaderPolicyB), h.Current().Hash)

	h.poll(t.Context())
	require.Equal(t, sha(loaderPolicyAFull), h.Current().Hash, "the complete file is published once stable")
	require.Equal(t, Grants{"*": models.RoleAdmin}, h.Resolve([]string{"admins"}, ""))
	got := events.all()
	require.Len(t, got, 1)
	require.True(t, got[0].OK)
	require.Equal(t, sha(loaderPolicyAFull), got[0].Revision)
}

// TestPollDiscardsAReadTheFileChangedUnder covers a write that lands while the
// poller is reading: the re-stat after the read sees the size move, and the
// read is discarded without even counting as a first sighting.
func TestPollDiscardsAReadTheFileChangedUnder(t *testing.T) {
	t.Parallel()
	h, path, events := loadTestHolder(t, loaderPolicyB, HolderOptions{})

	require.NoError(t, os.WriteFile(path, []byte(loaderPolicyA), 0o600))
	h.afterRead = func() { appendToFile(t, path, loaderPolicyAFull[len(loaderPolicyA):]) }
	h.poll(t.Context())
	h.afterRead = nil
	require.Empty(t, h.pendingKey, "an unstable read is not a sighting")
	require.Empty(t, events.all())

	h.poll(t.Context())
	require.Equal(t, sha(loaderPolicyB), h.Current().Hash)
	h.poll(t.Context())
	require.Equal(t, sha(loaderPolicyAFull), h.Current().Hash)
	require.Len(t, events.all(), 1)
}

// An observed write between two matching reads resets the stability gate:
// the matching reads were not consecutive, so the second one is only a new
// first sighting.
func TestPollUnstableReadResetsAPendingSighting(t *testing.T) {
	t.Parallel()
	h, path, events := loadTestHolder(t, loaderPolicyB, HolderOptions{})

	require.NoError(t, os.WriteFile(path, []byte(loaderPolicyA), 0o600))
	h.poll(t.Context())
	require.NotEmpty(t, h.pendingKey, "the first stable read of a change is a sighting")

	h.afterRead = func() { appendToFile(t, path, loaderPolicyAFull[len(loaderPolicyA):]) }
	h.poll(t.Context())
	h.afterRead = nil
	require.Empty(t, h.pendingKey, "an unstable read discards the earlier sighting")

	require.NoError(t, os.WriteFile(path, []byte(loaderPolicyA), 0o600))
	h.poll(t.Context())
	require.Equal(t, sha(loaderPolicyB), h.Current().Hash, "matching reads separated by a write must not publish")
	require.Empty(t, events.all())

	h.poll(t.Context())
	require.Equal(t, sha(loaderPolicyA), h.Current().Hash)
	require.Len(t, events.all(), 1)
}

// An API reload acts on one read, so it re-reads (briefly) when the file
// changed under it, and refuses a file that keeps changing.
func TestAPIReloadSettlesAReadTheFileChangedUnder(t *testing.T) {
	t.Parallel()
	h, path, events := loadTestHolder(t, loaderPolicyB, HolderOptions{})
	good := h.Current()

	require.NoError(t, os.WriteFile(path, []byte(loaderPolicyA), 0o600))
	reads := 0
	h.afterRead = func() {
		reads++
		if reads == 1 {
			appendToFile(t, path, loaderPolicyAFull[len(loaderPolicyA):])
		}
	}
	result, err := h.Reload(t.Context(), ReloadRequest{Actor: "a"})
	require.NoError(t, err)
	require.Equal(t, 2, reads)
	require.Equal(t, sha(loaderPolicyAFull), result.Current.Hash, "the settled read is published, not the cut-off one")

	writePolicy(t, path, loaderPolicyB)
	h.afterRead = func() { appendToFile(t, path, "#\n") }
	result, err = h.Reload(t.Context(), ReloadRequest{Actor: "a"})
	h.afterRead = nil
	require.Error(t, err)
	require.Contains(t, err.Error(), "kept changing while it was read")
	require.Equal(t, sha(loaderPolicyAFull), result.Current.Hash, "a file that keeps changing is never published")
	require.NotSame(t, good, h.Current())
	got := events.all()
	require.Len(t, got, 2)
	require.False(t, got[1].OK)
}

func TestRunPollsUntilTheContextEnds(t *testing.T) {
	t.Parallel()
	h, path, events := loadTestHolder(t, loaderPolicyA, HolderOptions{})
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.Run(ctx, 5*time.Millisecond)
	}()

	writePolicy(t, path, loaderPolicyB)
	require.Eventually(t, func() bool { return h.Current().Hash == sha(loaderPolicyB) }, 5*time.Second, 5*time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the poller did not stop with its context")
	}
	require.Len(t, events.all(), 1)
}

func TestRunRefusesANonPositiveInterval(t *testing.T) {
	t.Parallel()
	h, _, _ := loadTestHolder(t, loaderPolicyA, HolderOptions{})
	for _, interval := range []time.Duration{0, -time.Second} {
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.Run(t.Context(), interval) // must return, not panic in NewTicker
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("Run(%s) did not return", interval)
		}
	}
	// An env holder has no file to poll.
	NewEnvHolder(nil, "", nil).Run(t.Context(), time.Millisecond)
}

// TestOverlappingReloadsNeverPublishAStaleRevision is the D6 regression: a
// poll that read revision A must not publish A over a revision B that an API
// reload published meanwhile, which would silently restore revoked access.
// The poller is parked mid-validation (holding the writer lock) while the
// file changes to B and an API reload starts; the API reload must wait, then
// read and publish B. -race alone cannot see this logical race.
func TestOverlappingReloadsNeverPublishAStaleRevision(t *testing.T) {
	t.Parallel()
	h, path, events := loadTestHolder(t, loaderPolicyB, HolderOptions{})

	parked := make(chan struct{})
	release := make(chan struct{})
	h.afterParse = func(trigger string) {
		if trigger == TriggerPoll {
			close(parked)
			<-release
		}
	}

	writePolicy(t, path, loaderPolicyA) // A grants viewers
	h.poll(context.Background())        // first sighting: A is pending, nothing parsed
	pollDone := make(chan struct{})
	go func() {
		defer close(pollDone)
		h.poll(context.Background())
	}()
	<-parked // the poller has read and validated A and holds the lock

	writePolicy(t, path, loaderPolicyB) // B revokes viewers
	type reloadOutcome struct {
		result ReloadResult
		err    error
	}
	apiDone := make(chan reloadOutcome, 1)
	go func() {
		result, err := h.Reload(context.Background(), ReloadRequest{Actor: "csk_live_admin"})
		apiDone <- reloadOutcome{result, err}
	}()

	select {
	case <-apiDone:
		t.Fatal("the API reload published while the poller held the writer lock")
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	<-pollDone
	outcome := <-apiDone
	require.NoError(t, outcome.err)
	require.True(t, outcome.result.Changed)

	require.Equal(t, sha(loaderPolicyB), h.Current().Hash, "B must still be current")
	require.Empty(t, h.Resolve([]string{"viewers"}, ""), "the revoked grant must stay revoked")
	got := events.all()
	require.Len(t, got, 2)
	require.Equal(t, TriggerPoll, got[0].Trigger)
	require.Equal(t, sha(loaderPolicyA), got[0].Revision)
	require.Equal(t, TriggerAPI, got[1].Trigger)
	require.Equal(t, sha(loaderPolicyA), got[1].PreviousRevision)
	require.Equal(t, sha(loaderPolicyB), got[1].Revision)
}

func TestConcurrentResolveDuringReloads(t *testing.T) {
	t.Parallel()
	h, path, _ := loadTestHolder(t, loaderPolicyA, HolderOptions{})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	var readers sync.WaitGroup
	for range 8 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for ctx.Err() == nil {
				grants := h.Resolve([]string{"viewers", "admins"}, "")
				role := grants.ClusterRole()
				if role != models.RoleViewer && role != models.RoleAdmin {
					t.Errorf("a reader saw a torn revision: %v", grants)
					return
				}
				_, _ = h.Namespace("marketing")
				_ = h.Current().Hash
				_ = h.LastReload()
			}
		}()
	}
	go h.Run(ctx, time.Millisecond)
	for i := range 50 {
		body := loaderPolicyA
		if i%2 == 0 {
			body = loaderPolicyB
		}
		writePolicy(t, path, body)
		_, err := h.Reload(ctx, ReloadRequest{Actor: "a"})
		require.NoError(t, err)
	}
	cancel()
	readers.Wait()
}

func TestDefaultRoleFallbackWithFile(t *testing.T) {
	t.Parallel()
	h, _, _ := loadTestHolder(t, loaderPolicyA, HolderOptions{DefaultRole: models.RoleRunner})
	require.Equal(t, models.RoleRunner, h.DefaultRole())
	require.Equal(t, Grants{"*": models.RoleRunner}, h.Resolve([]string{"nobody"}, ""))
	// A confined match is not empty, so the default never joins it.
	require.Equal(t, Grants{"marketing": models.RoleOperator}, h.Resolve([]string{"marketing-eng"}, ""))
}

func TestFileGroupsAreMatchedByteExact(t *testing.T) {
	t.Parallel()
	h, _, _ := loadTestHolder(t, loaderHeader+`bindings:
  - subjects: {groups: [" CN=Caesium Admins,OU=Groups "]}
    role: admin
    namespaces: ["*"]
`, HolderOptions{})
	require.Equal(t, Grants{"*": models.RoleAdmin}, h.Resolve([]string{" CN=Caesium Admins,OU=Groups "}, ""))
	require.Empty(t, h.Resolve([]string{"CN=Caesium Admins,OU=Groups"}, ""), "file groups are never trimmed")
	require.Empty(t, h.Resolve([]string{" cn=caesium admins,ou=groups "}, ""))
}

func TestHolderWarnings(t *testing.T) {
	t.Parallel()
	body := loaderHeader + `namespaces:
  marketing: {}
bindings:
  - subjects: {groups: [marketing-eng, ghosts, "*"]}
    role: viewer
    namespaces: [marketing]
  - subjects: {users: [alice@example.com]}
    role: operator
    namespaces: [marketing]
`
	known := func(context.Context) (map[string]struct{}, error) {
		return map[string]struct{}{"marketing-eng": {}, "other": {}}, nil
	}

	h, _, _ := loadTestHolder(t, body, HolderOptions{KnownGroups: known})
	require.Equal(t, []string{
		`namespace "marketing" has no secret rules; all secrets are allowed`,
		"bindings[1].subjects.users: no enabled identity provider supplies a trusted email (only OIDC with email_verified=true does), so these users never match",
		`bindings[0].subjects.groups: "ghosts" matches no known user's groups`,
	}, h.Current().Warnings)

	trusted, _, _ := loadTestHolder(t, body, HolderOptions{KnownGroups: known, TrustedEmailProvider: true})
	require.Equal(t, []string{
		`namespace "marketing" has no secret rules; all secrets are allowed`,
		`bindings[0].subjects.groups: "ghosts" matches no known user's groups`,
	}, trusted.Current().Warnings)

	// A failing user lookup skips the group check; it never fails a load.
	broken := func(context.Context) (map[string]struct{}, error) { return nil, errors.New("database is locked") }
	h, _, _ = loadTestHolder(t, body, HolderOptions{KnownGroups: broken, TrustedEmailProvider: true})
	require.Equal(t, []string{`namespace "marketing" has no secret rules; all secrets are allowed`}, h.Current().Warnings)

	// No lookup at all when every group is the wildcard.
	called := false
	probe := func(context.Context) (map[string]struct{}, error) { called = true; return nil, nil }
	h, _, _ = loadTestHolder(t, loaderHeader+"bindings:\n  - subjects: {groups: [\"*\"]}\n    role: viewer\n    namespaces: [\"*\"]\n", HolderOptions{KnownGroups: probe})
	require.False(t, called)
	require.Equal(t, []string{}, h.Current().Warnings)
}

func TestFailedReloadLeavesWarningsAndUnchangedReloadRefreshesThem(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	knownSet := map[string]struct{}{}
	known := func(context.Context) (map[string]struct{}, error) {
		mu.Lock()
		defer mu.Unlock()
		out := make(map[string]struct{}, len(knownSet))
		for group := range knownSet {
			out[group] = struct{}{}
		}
		return out, nil
	}
	h, path, _ := loadTestHolder(t, loaderPolicyA, HolderOptions{KnownGroups: known})
	good := h.Current()
	require.Equal(t, []string{
		`bindings[0].subjects.groups: "viewers" matches no known user's groups`,
		`bindings[1].subjects.groups: "marketing-eng" matches no known user's groups`,
	}, good.Warnings)

	writePolicy(t, path, loaderPolicyInvalid)
	_, err := h.Reload(t.Context(), ReloadRequest{Actor: "a"})
	require.Error(t, err)
	require.Same(t, good, h.Current(), "nothing of a failed reload is applied")

	writePolicy(t, path, loaderPolicyA)
	mu.Lock()
	knownSet["viewers"] = struct{}{}
	mu.Unlock()
	result, err := h.Reload(t.Context(), ReloadRequest{Actor: "a"})
	require.NoError(t, err)
	require.False(t, result.Changed)
	require.Equal(t, good.Hash, h.Current().Hash)
	require.Equal(t, good.LoadedAt, h.Current().LoadedAt)
	require.Same(t, good.Policy, h.Current().Policy)
	require.Equal(t, []string{`bindings[1].subjects.groups: "marketing-eng" matches no known user's groups`}, h.Current().Warnings)
	require.Equal(t, []string{
		`bindings[0].subjects.groups: "viewers" matches no known user's groups`,
		`bindings[1].subjects.groups: "marketing-eng" matches no known user's groups`,
	}, good.Warnings, "published revisions are never mutated")
}

func TestEnvHolder(t *testing.T) {
	t.Parallel()
	view := EmptyPolicy()
	h := NewEnvHolder(view, models.RoleViewer, fixedResolver{Grants{"*": models.RoleOperator}})
	require.False(t, h.FileBacked())
	require.Empty(t, h.Path())
	require.Equal(t, models.RoleViewer, h.DefaultRole())
	rev := h.Current()
	require.Equal(t, SourceEnv, rev.Source)
	require.Empty(t, rev.Hash)
	require.Same(t, view, rev.Policy)
	require.Equal(t, []string{}, rev.Warnings)
	require.Equal(t, Grants{"*": models.RoleOperator}, h.Resolve([]string{"x"}, ""), "resolution is the delegate's")

	result, err := h.Reload(t.Context(), ReloadRequest{})
	require.ErrorIs(t, err, ErrNoPolicyFile)
	require.Same(t, rev, result.Current)

	noSource := NewEnvHolder(nil, "", nil)
	require.Equal(t, EmptyPolicy(), noSource.Current().Policy)
	require.Equal(t, Grants{}, noSource.Resolve([]string{"admins"}, "a@example.com"))
	_, declared := noSource.Namespace(DefaultNamespace)
	require.True(t, declared)
}

// fixedResolver returns fixed grants (tests only).
type fixedResolver struct{ grants Grants }

func (s fixedResolver) Resolve([]string, string) Grants {
	out := make(Grants, len(s.grants))
	for ns, role := range s.grants {
		out[ns] = role
	}
	return out
}

func TestNilHolderIsSafe(t *testing.T) {
	t.Parallel()
	var h *Holder
	var src Source = h
	require.Equal(t, Grants{}, src.Resolve([]string{"admins"}, "a@example.com"))
	require.Nil(t, h.Current())
	require.Nil(t, h.LastReload())
	require.False(t, h.FileBacked())
	require.Empty(t, h.Path())
	require.Empty(t, h.DefaultRole())
	h.SetObserver(func(ReloadEvent) {})
	h.Run(t.Context(), time.Second)
	_, err := h.Reload(t.Context(), ReloadRequest{})
	require.ErrorIs(t, err, ErrNoPolicyFile)
}

func TestLintWarnings(t *testing.T) {
	t.Parallel()
	p, err := Parse([]byte(loaderHeader + `namespaces:
  marketing: {}
bindings:
  - subjects: {groups: [a]}
    role: viewer
    namespaces: [marketing]
  - subjects: {users: [alice@example.com]}
    role: viewer
    namespaces: ["*"]
`))
	require.NoError(t, err)
	require.Equal(t, []string{
		`namespace "marketing" has no secret rules; all secrets are allowed`,
		"bindings[1].subjects.users: only OIDC logins with email_verified=true supply a trusted email; SAML and LDAP logins never match user bindings",
	}, LintWarnings(p))

	clean, err := Parse([]byte(loaderHeader))
	require.NoError(t, err)
	require.Equal(t, []string{}, LintWarnings(clean))
}

func TestReadFileHashesExactBytes(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "p.yaml")
	require.NoError(t, os.WriteFile(path, []byte(loaderPolicyA), 0o600))
	data, hash, err := ReadFile(path)
	require.NoError(t, err)
	require.Equal(t, loaderPolicyA, string(data))
	require.Equal(t, sha(loaderPolicyA), hash)
	require.Equal(t, hash, Hash(data))
	require.Len(t, hash, 64)
}

// TestReloadWithUnsupportedSecretProviderKeepsLastGood pins that a secret rule
// naming a provider outside env/k8s/kubernetes/vault is a validation error at
// reload, so the last good revision (and its enforced rules) keeps serving.
func TestReloadWithUnsupportedSecretProviderKeepsLastGood(t *testing.T) {
	h, path, _ := loadTestHolder(t, loaderPolicyA, HolderOptions{})
	good := h.Current()

	writePolicy(t, path, strings.Replace(loaderPolicyA, `"env/MARKETING_*"`, `"env/MARKETING_*", "aws/marketing/token"`, 1))
	result, err := h.Reload(t.Context(), ReloadRequest{Actor: "csk_live_admin"})
	require.ErrorContains(t, err, `unsupported provider "aws"`)
	require.False(t, result.Changed)
	require.Same(t, good, h.Current(), "an unsupported provider never replaces the last good revision")
	ns, declared := h.Namespace("marketing")
	require.True(t, declared)
	require.Equal(t, []string{"env/MARKETING_*"}, ns.Secrets.Allow)

	// The kubernetes alias is a supported provider and is served verbatim.
	writePolicy(t, path, strings.Replace(loaderPolicyA, `"env/MARKETING_*"`, `"env/MARKETING_*", "kubernetes/marketing-*"`, 1))
	result, err = h.Reload(t.Context(), ReloadRequest{Actor: "csk_live_admin"})
	require.NoError(t, err)
	require.True(t, result.Changed)
	ns, _ = h.Namespace("marketing")
	require.Equal(t, []string{"env/MARKETING_*", "kubernetes/marketing-*"}, ns.Secrets.Allow)
}
