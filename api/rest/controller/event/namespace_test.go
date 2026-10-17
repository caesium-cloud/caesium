package event

import (
	"bufio"
	"context"
	"encoding/json"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"

	authmw "github.com/caesium-cloud/caesium/api/middleware"
	eventsvc "github.com/caesium-cloud/caesium/api/rest/service/event"
	iauth "github.com/caesium-cloud/caesium/internal/auth"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/jobdef/testutil"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
	"gorm.io/gorm"
)

// TestRestrictedStreamTypeProducersAudited pins the producers of every event
// type a namespace-confined subscriber may receive (restrictedStreamTypes).
// A producer is a non-test source file that names an allowed type and builds
// an event (event.Event / models.ExecutionEvent literals or the store's task
// event recorders). Each listed file's payloads were audited to describe only
// the run and job the event names; a new or moved producer fails here until
// it is audited and listed (identity-and-access B4).
func TestRestrictedStreamTypeProducersAudited(t *testing.T) {
	audited := []string{
		"internal/incident/subscriber.go",
		"internal/notification/watcher.go",
		"internal/replay/replay.go",
		"internal/run/fanout.go",
		"internal/run/store.go",
		"internal/run/store_instance.go",
		"internal/run/store_owner_support.go",
		"internal/worker/claimer.go",
	}
	names := make([]string, 0, len(restrictedStreamTypes))
	for typ := range restrictedStreamTypes {
		constant := "Type"
		for _, part := range strings.Split(string(typ), "_") {
			constant += strings.ToUpper(part[:1]) + part[1:]
		}
		names = append(names, constant)
	}
	allowed := regexp.MustCompile(`event\.(` + strings.Join(names, "|") + `)\b`)
	builds := regexp.MustCompile(`event\.Event\{|ExecutionEvent\{|record(TaskRun|Task)EventTx\(`)

	root := repoRoot(t)
	var producers []string
	for _, dir := range []string{"api", "cmd", "internal", "pkg"} {
		require.NoError(t, filepath.WalkDir(filepath.Join(root, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return err
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			if allowed.Match(data) && builds.Match(data) {
				rel, err := filepath.Rel(root, path)
				if err != nil {
					return err
				}
				producers = append(producers, filepath.ToSlash(rel))
			}
			return nil
		}))
	}
	sort.Strings(producers)
	require.Equal(t, audited, producers, "audit the payloads of every new producer of an allow-listed stream type, then list it")

	// Every allow-listed constant exists (the derived names must match).
	for _, name := range names {
		require.Contains(t, readFile(t, filepath.Join(root, "internal/event/bus.go")), "\t"+name+" ", name)
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	require.NoError(t, err)
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		require.NotEqual(t, dir, parent, "go.mod not found")
		dir = parent
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	require.NoError(t, err)
	return string(data)
}

func TestRestrictedStreamFilterTypes(t *testing.T) {
	all, ok := restrictedStreamFilterTypes(nil)
	require.True(t, ok)
	require.Len(t, all, len(restrictedStreamTypes))
	for _, typ := range all {
		require.True(t, restrictedStreamTypes[typ])
	}
	narrowed, ok := restrictedStreamFilterTypes([]event.Type{event.TypeRunStarted, event.TypeDatasetHeld, event.TypeRunStarted})
	require.True(t, ok)
	require.Equal(t, []event.Type{event.TypeRunStarted}, narrowed)
	_, ok = restrictedStreamFilterTypes([]event.Type{event.TypeJobPaused, event.TypeSLAMissed})
	require.False(t, ok, "only withheld types requested: nothing is visible")
}

// streamFixture holds a test database with a marketing and a finance run.
type streamFixture struct {
	db        *gorm.DB
	store     *event.Store
	auth      *iauth.Service
	jobID     uuid.UUID
	mktRun    uuid.UUID
	finRun    uuid.UUID
	mktKey    string
	cluster   string
	finSecret string
}

func newStreamFixture(t *testing.T) *streamFixture {
	t.Helper()
	db := testutil.OpenTestDB(t)
	t.Cleanup(func() { testutil.CloseDB(db) })
	f := &streamFixture{db: db, store: event.NewStore(db), auth: iauth.NewService(db), jobID: uuid.New(), finSecret: "finance-private-payload-" + uuid.NewString()}
	now := time.Now().UTC()
	require.NoError(t, db.Create(&models.Job{ID: f.jobID, Alias: "stream-ns", Namespace: "marketing", TriggerID: uuid.New(), CreatedAt: now, UpdatedAt: now}).Error)
	run := func(namespace string) uuid.UUID {
		id := uuid.New()
		require.NoError(t, db.Create(&models.JobRun{ID: id, JobID: f.jobID, Namespace: namespace, Status: "running", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
		return id
	}
	f.mktRun, f.finRun = run("marketing"), run("finance")
	f.mktKey = guardViewerKey(t, db, `["marketing"]`)
	f.cluster = guardViewerKey(t, db, "")
	return f
}

func (f *streamFixture) append(t *testing.T, evt event.Event) event.Event {
	t.Helper()
	tx := f.db.Begin()
	require.NoError(t, f.store.AppendTx(tx, &evt))
	require.NoError(t, tx.Commit().Error)
	return evt
}

func (f *streamFixture) controller(bus event.Bus) *Controller {
	ctrl := New(bus).WithAuthService(f.auth)
	ctrl.storeOnce.Do(func() {})
	ctrl.store = f.store
	return ctrl
}

// stream serves GET /v1/events?query behind the real auth middleware with the
// collection guard on until the context expires, and returns the streamed
// events and the middleware's error.
func (f *streamFixture) stream(t *testing.T, ctrl *Controller, key, query string, d time.Duration) ([]event.Event, error) {
	t.Helper()
	deps := authmw.AuthDeps{Service: f.auth, Auditor: iauth.NewAuditLogger(f.db), Limiter: iauth.NewRateLimiter(1000, time.Minute), RequireCollectionFilter: true}
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/v1/events"+query, nil)
	req.Header.Set("Authorization", "Bearer "+key)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	c.InitializeRoute(&echo.RouteInfo{Path: "/v1/events", Method: http.MethodGet}, &echo.PathValues{})
	done := make(chan error, 1)
	go func() { done <- authmw.Auth(deps)(ctrl.Stream)(c) }()
	err := <-done
	var events []event.Event
	scanner := bufio.NewScanner(strings.NewReader(rec.Body.String()))
	scanner.Buffer(make([]byte, 0, 1<<20), 1<<20)
	for scanner.Scan() {
		if data, ok := strings.CutPrefix(scanner.Text(), "data: "); ok {
			var evt event.Event
			require.NoError(t, json.Unmarshal([]byte(data), &evt))
			events = append(events, evt)
		}
	}
	return events, err
}

func eventRuns(events []event.Event) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(events))
	for _, evt := range events {
		out = append(out, evt.RunID)
	}
	return out
}

// TestEventStreamCollectionGuard serves the real GET /v1/events handler as a
// confined subscriber: the catch-up and the live feed carry only allow-listed
// events of runs in its namespaces (never runless events, never another
// namespace's payload), a run_id in another namespace is an audited 403, and
// an unrestricted subscriber is unchanged (B4).
func TestEventStreamCollectionGuard(t *testing.T) {
	f := newStreamFixture(t)
	payload := json.RawMessage(`{"secret":"` + f.finSecret + `"}`)
	// The job's current labels ride on run and task payloads; a confined
	// subscriber never receives them (they may belong to a new owner).
	labelled := json.RawMessage(`{"id":"` + f.mktRun.String() + `","job_labels":{"owner":"` + f.finSecret + `"},"tasks":[{"name":"t","job_labels":{"owner":"` + f.finSecret + `"}}],"attempt":12345678901234567}`)
	f.append(t, event.Event{Type: event.TypeRunStarted, JobID: f.jobID, RunID: f.mktRun, Payload: labelled})
	// A run admission refused (its consumed dataset is held) names the hold.
	held := uuid.New()
	now := time.Now().UTC()
	require.NoError(t, f.db.Create(&models.JobRun{ID: held, JobID: f.jobID, Namespace: "marketing", Status: "skipped",
		SkipReason: "dataset_hold:/finance-dataset", StartedAt: now, CreatedAt: now, UpdatedAt: now}).Error)
	f.append(t, event.Event{Type: event.TypeTaskSkipped, JobID: f.jobID, RunID: held, Payload: json.RawMessage(`{"error":"dataset_hold:/finance-dataset hold=` + f.finSecret + `"}`)})
	f.append(t, event.Event{Type: event.TypeRunStarted, JobID: f.jobID, RunID: f.finRun, Payload: payload})
	f.append(t, event.Event{Type: event.TypeJobPaused, JobID: f.jobID, Payload: payload})
	f.append(t, event.Event{Type: event.TypeSLAMissed, JobID: f.jobID, RunID: f.mktRun, Payload: payload})

	bus := event.New()
	ctrl := f.controller(bus)
	go func() {
		// Live events published while the subscriber is connected.
		time.Sleep(150 * time.Millisecond)
		bus.Publish(event.Event{Type: event.TypeTaskStarted, JobID: f.jobID, RunID: f.finRun, Payload: payload})
		bus.Publish(event.Event{Type: event.TypeTaskSkipped, JobID: f.jobID, RunID: held, Payload: payload})
		bus.Publish(event.Event{Type: event.TypeJobUnpaused, JobID: f.jobID, Payload: payload})
		bus.Publish(event.Event{Type: event.TypeTaskStarted, JobID: f.jobID, RunID: f.mktRun, Payload: labelled})
	}()
	events, err := f.stream(t, ctrl, f.mktKey, "", 600*time.Millisecond)
	require.NoError(t, err, "the collection guard must see the stream filtered")
	require.Equal(t, []uuid.UUID{f.mktRun, f.mktRun}, eventRuns(events))
	require.Equal(t, event.TypeRunStarted, events[0].Type)
	require.Equal(t, event.TypeTaskStarted, events[1].Type)
	for _, evt := range events {
		require.NotContains(t, string(evt.Payload), f.finSecret)
		require.Contains(t, string(evt.Payload), `"attempt":12345678901234567`, "the rest of the payload is unchanged")
	}

	// A run_id in another namespace is refused before anything streams.
	_, err = f.stream(t, ctrl, f.mktKey, "?run_id="+f.finRun.String(), time.Second)
	require.Error(t, err)
	require.Equal(t, http.StatusForbidden, echo.StatusCode(err))
	events, err = f.stream(t, ctrl, f.mktKey, "?run_id="+f.mktRun.String(), 300*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{f.mktRun}, eventRuns(events))

	// Only withheld types requested: the stream stays open and empty.
	events, err = f.stream(t, ctrl, f.mktKey, "?types=job_paused,sla_missed", 300*time.Millisecond)
	require.NoError(t, err)
	require.Empty(t, events)

	events, err = f.stream(t, f.controller(nil), f.cluster, "", 300*time.Millisecond)
	require.NoError(t, err)
	require.Len(t, events, 5, "an unrestricted subscriber gets every persisted event")
	require.Contains(t, string(events[0].Payload), f.finSecret, "and its payloads unredacted")
}

// TestEventStreamCatchupEndsOnHiddenTail is the regression for a catch-up that
// never ended: when the newest event before the captured high-water mark is
// hidden from the subscriber and a visible event lands right after the mark,
// the bounded catch-up finishes and the new event arrives live, once.
func TestEventStreamCatchupEndsOnHiddenTail(t *testing.T) {
	f := newStreamFixture(t)
	f.append(t, event.Event{Type: event.TypeRunStarted, JobID: f.jobID, RunID: f.mktRun})
	f.append(t, event.Event{Type: event.TypeRunStarted, JobID: f.jobID, RunID: f.finRun}) // the hidden tail
	bus := event.New()
	prev := streamCatchupCaptured
	t.Cleanup(func() { streamCatchupCaptured = prev })
	streamCatchupCaptured = func() {
		visible := f.append(t, event.Event{Type: event.TypeTaskStarted, JobID: f.jobID, RunID: f.mktRun})
		bus.Publish(visible)
	}
	events, err := f.stream(t, f.controller(bus), f.mktKey, "", 800*time.Millisecond)
	require.NoError(t, err)
	require.Equal(t, []event.Type{event.TypeRunStarted, event.TypeTaskStarted}, []event.Type{events[0].Type, events[len(events)-1].Type})
	require.Len(t, events, 2, "the visible event after the mark arrives exactly once")
}

// TestIngestedEventsCollectionGuard serves the real GET /v1/events/ingested
// handler as a confined principal: an ingested event is shown only when every
// delivery it caused started runs, all in the caller's namespaces, with no
// skip and no error. Foreign, mixed, skipped, failed and unrouted events (and
// their payloads) stay hidden (B4).
func TestIngestedEventsCollectionGuard(t *testing.T) {
	f := newStreamFixture(t)
	prev := ingestedEventService
	ingestedEventService = func(ctx context.Context) eventsvc.Service { return eventsvc.NewWithDatabase(ctx, f.db) }
	t.Cleanup(func() { ingestedEventService = prev })

	now := time.Now().UTC()
	trigger := uuid.New()
	ingested := func(name string, matches ...models.EventTriggerMatch) {
		id := uuid.New()
		require.NoError(t, f.db.Create(&models.IngestedEvent{ID: id, Type: "test." + name, Source: "b4", Data: datatypes.JSON(`{"marker":"` + name + `-` + f.finSecret + `"}`), CreatedAt: now}).Error)
		for _, m := range matches {
			m.ID, m.EventID, m.TriggerID, m.MatchedAt = uuid.New(), id, trigger, now
			require.NoError(t, f.db.Create(&m).Error)
		}
	}
	runs := func(ids ...uuid.UUID) datatypes.JSON {
		values := make([]string, 0, len(ids))
		for _, id := range ids {
			values = append(values, id.String())
		}
		data, err := json.Marshal(values)
		require.NoError(t, err)
		return datatypes.JSON(data)
	}
	ingested("owned", models.EventTriggerMatch{RunsStarted: runs(f.mktRun)})
	ingested("foreign", models.EventTriggerMatch{RunsStarted: runs(f.finRun)})
	ingested("mixed", models.EventTriggerMatch{RunsStarted: runs(f.mktRun, f.finRun)})
	ingested("skipped", models.EventTriggerMatch{Skipped: true, SkipReason: "job " + uuid.NewString() + " paused"})
	ingested("partly-skipped", models.EventTriggerMatch{RunsStarted: runs(f.mktRun), SkipReason: "another job was paused"})
	ingested("errored", models.EventTriggerMatch{RunsStarted: runs(f.mktRun), Error: "boom"})
	ingested("two-deliveries", models.EventTriggerMatch{RunsStarted: runs(f.mktRun)}, models.EventTriggerMatch{Skipped: true})
	ingested("unrouted")
	ingested("unknown-run", models.EventTriggerMatch{RunsStarted: runs(uuid.New())})

	list := func(key string) ([]string, string) {
		rec := serveCollectionGuarded(t, f.db, key, "/v1/events/ingested", "/v1/events/ingested?limit=50", New(nil).ListIngested)
		require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
		var body []models.IngestedEvent
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
		names := make([]string, 0, len(body))
		for _, entry := range body {
			names = append(names, strings.TrimPrefix(entry.Type, "test."))
		}
		sort.Strings(names)
		return names, rec.Body.String()
	}
	names, raw := list(f.mktKey)
	require.Equal(t, []string{"owned"}, names)
	require.Equal(t, 1, strings.Count(raw, f.finSecret), "only the owned event's body is returned")
	names, _ = list(guardViewerKey(t, f.db, `["marketing","finance"]`))
	require.Equal(t, []string{"foreign", "mixed", "owned"}, names)
	names, _ = list(f.cluster)
	require.Len(t, names, 9, "an unrestricted principal lists every ingested event")
}
