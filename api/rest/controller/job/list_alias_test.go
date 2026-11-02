package job

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caesium-cloud/caesium/api/middleware"
	jsvc "github.com/caesium-cloud/caesium/api/rest/service/job"
	"github.com/caesium-cloud/caesium/internal/event"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestNarrowJobListAliases(t *testing.T) {
	aliases, empty := narrowJobListAliases(nil, nil)
	require.False(t, empty)
	require.Nil(t, aliases)

	aliases, empty = narrowJobListAliases([]string{"a", "b"}, nil)
	require.False(t, empty)
	require.Equal(t, []string{"a", "b"}, aliases)

	aliases, empty = narrowJobListAliases(nil, []string{"nightly"})
	require.False(t, empty)
	require.Equal(t, []string{"nightly"}, aliases)

	aliases, empty = narrowJobListAliases([]string{"nightly", "other"}, []string{"nightly"})
	require.False(t, empty)
	require.Equal(t, []string{"nightly"}, aliases, "?alias= narrows inside the allowlist")

	aliases, empty = narrowJobListAliases([]string{"nightly"}, []string{"foreign"})
	require.True(t, empty)
	require.Nil(t, aliases, "an alias outside the allowlist must not drop the predicate")
}

func TestParseListRequestAlias(t *testing.T) {
	parsed, err := parseListRequest(newListContext(t, "/v1/jobs?alias=nightly&namespace=marketing"))
	require.NoError(t, err)
	require.Equal(t, []string{"nightly"}, parsed.Aliases)
	require.Equal(t, "marketing", parsed.Namespace)

	parsed, err = parseListRequest(newListContext(t, "/v1/jobs?alias=%20"))
	require.NoError(t, err)
	require.Empty(t, parsed.Aliases)
}

func TestListAliasNarrowsWithinAllowedSet(t *testing.T) {
	fake := &aliasListFake{}
	prev := jobListService
	jobListService = func(context.Context) jsvc.Job { return fake }
	t.Cleanup(func() { jobListService = prev })

	rec := serveList(t, fake, "/v1/jobs?alias=nightly&namespace=marketing", []string{"nightly", "other"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.True(t, fake.called)
	require.Equal(t, []string{"nightly"}, fake.got.Aliases)
	require.Equal(t, "marketing", fake.got.Namespace)

	fake.called = false
	fake.got = nil
	rec = serveList(t, fake, "/v1/jobs?alias=foreign", []string{"nightly"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.JSONEq(t, "[]", rec.Body.String())
	require.False(t, fake.called, "an out-of-scope alias must not be listed")

	fake.called = false
	rec = serveList(t, fake, "/v1/jobs", []string{"nightly", "other"})
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, []string{"nightly", "other"}, fake.got.Aliases)
}

func newListContext(t *testing.T, target string) *echo.Context {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	return echo.New().NewContext(req, httptest.NewRecorder())
}

func serveList(t *testing.T, fake *aliasListFake, target string, allowed []string) *httptest.ResponseRecorder {
	t.Helper()
	require.NotNil(t, fake)
	req := httptest.NewRequest(http.MethodGet, target, nil)
	rec := httptest.NewRecorder()
	c := echo.New().NewContext(req, rec)
	if allowed != nil {
		c.Set(middleware.ContextKeyAllowedJobAliases, allowed)
	}
	require.NoError(t, List(c))
	return rec
}

// aliasListFake records the list request. Every other method fails the test
// if a list handler starts using it.
type aliasListFake struct {
	called bool
	got    *jsvc.ListRequest
}

func (f *aliasListFake) WithDatabase(*gorm.DB) jsvc.Job { return f }
func (f *aliasListFake) SetBus(event.Bus)               {}
func (f *aliasListFake) List(req *jsvc.ListRequest) (models.Jobs, error) {
	f.called = true
	f.got = req
	return models.Jobs{}, nil
}
func (f *aliasListFake) ListRecentRuns([]uuid.UUID, int) (map[uuid.UUID][]jsvc.RunListSummary, error) {
	return map[uuid.UUID][]jsvc.RunListSummary{}, nil
}
func (f *aliasListFake) Get(uuid.UUID) (*models.Job, error) { return nil, nil }
func (f *aliasListFake) GetByIDPrefix(string) (*models.Job, error) {
	return nil, nil
}
func (f *aliasListFake) Queue(uuid.UUID) ([]jsvc.QueueItem, error)  { return nil, nil }
func (f *aliasListFake) CancelQueuedRun(uuid.UUID, uuid.UUID) error { return nil }
func (f *aliasListFake) Create(*jsvc.CreateRequest) (*models.Job, error) {
	return nil, nil
}
func (f *aliasListFake) Delete(uuid.UUID) error { return nil }
func (f *aliasListFake) SetPaused(uuid.UUID, bool) (*models.Job, error) {
	return nil, nil
}
