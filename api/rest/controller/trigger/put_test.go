package trigger

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	triggersvc "github.com/caesium-cloud/caesium/api/rest/service/trigger"
	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/pkg/env"
	"github.com/google/uuid"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

type stubTriggerService struct {
	trigger  *models.Trigger
	createFn func(*triggersvc.CreateRequest) (*models.Trigger, error)
	updateFn func(uuid.UUID, *triggersvc.UpdateRequest) (*models.Trigger, error)
}

var triggerControllerTestMu sync.Mutex

func (s *stubTriggerService) WithDatabase(*gorm.DB) triggersvc.Trigger { return s }
func (s *stubTriggerService) List(*triggersvc.ListRequest) (models.Triggers, error) {
	return nil, nil
}
func (s *stubTriggerService) ListByPath(string) (models.Triggers, error) { return nil, nil }
func (s *stubTriggerService) ListByEventPattern(string, string) (models.Triggers, error) {
	return nil, nil
}
func (s *stubTriggerService) Get(id uuid.UUID) (*models.Trigger, error) {
	if s.trigger != nil && s.trigger.ID == id {
		return s.trigger, nil
	}
	return nil, gorm.ErrRecordNotFound
}
func (s *stubTriggerService) Create(req *triggersvc.CreateRequest) (*models.Trigger, error) {
	if s.createFn != nil {
		return s.createFn(req)
	}
	return nil, nil
}
func (s *stubTriggerService) Update(id uuid.UUID, req *triggersvc.UpdateRequest) (*models.Trigger, error) {
	if s.updateFn != nil {
		return s.updateFn(id, req)
	}
	return nil, nil
}
func (s *stubTriggerService) Delete(uuid.UUID) error { return nil }

func TestFireAcceptsOptionalParams(t *testing.T) {
	triggerControllerTestMu.Lock()
	defer triggerControllerTestMu.Unlock()

	created := &models.Trigger{
		ID:   uuid.New(),
		Type: models.TriggerTypeHTTP,
	}

	origTriggerSvcFactory := triggerServiceFactory
	origFire := fireHTTPTrigger
	defer func() {
		triggerServiceFactory = origTriggerSvcFactory
		fireHTTPTrigger = origFire
	}()

	triggerServiceFactory = func(context.Context) triggersvc.Trigger {
		return &stubTriggerService{trigger: created}
	}

	var (
		capturedParams   map[string]string
		capturedPriority string
	)
	fireHTTPTrigger = func(_ context.Context, trig *models.Trigger, params map[string]string, priority string) error {
		require.Equal(t, created.ID, trig.ID)
		capturedParams = params
		capturedPriority = priority
		return nil
	}

	body, err := json.Marshal(map[string]any{
		"params":   map[string]string{"branch": "main"},
		"priority": "high",
	})
	require.NoError(t, err)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	req.Header.Set("X-Caesium-API-Key", "test-key")
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: created.ID.String()}})
	t.Setenv("CAESIUM_MANUAL_TRIGGER_API_KEY", "test-key")
	require.NoError(t, env.Process())

	err = Fire(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusAccepted, rec.Code)
	require.Equal(t, map[string]string{"branch": "main"}, capturedParams)
	require.Equal(t, "high", capturedPriority)
}

func TestFireRejectsMissingAPIKey(t *testing.T) {
	triggerControllerTestMu.Lock()
	defer triggerControllerTestMu.Unlock()

	created := &models.Trigger{ID: uuid.New(), Type: models.TriggerTypeHTTP}
	origTriggerSvcFactory := triggerServiceFactory
	defer func() { triggerServiceFactory = origTriggerSvcFactory }()
	triggerServiceFactory = func(context.Context) triggersvc.Trigger {
		return &stubTriggerService{trigger: created}
	}

	t.Setenv("CAESIUM_MANUAL_TRIGGER_API_KEY", "test-key")
	require.NoError(t, env.Process())

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", nil)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: created.ID.String()}})

	err := Fire(c)
	require.Error(t, err)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusUnauthorized, httpErr.Code)
}

func TestParseOptionalFireRequestEnvelope(t *testing.T) {
	tests := []struct {
		name         string
		body         map[string]any
		wantParams   map[string]string
		wantPriority string
		wantErr      bool
	}{
		{
			name:         "priority without params",
			body:         map[string]any{"priority": "high"},
			wantPriority: "high",
		},
		{
			name:       "params without priority",
			body:       map[string]any{"params": map[string]string{"branch": "main"}},
			wantParams: map[string]string{"branch": "main"},
		},
		{
			name:         "params with priority",
			body:         map[string]any{"params": map[string]string{"branch": "main"}, "priority": "high"},
			wantParams:   map[string]string{"branch": "main"},
			wantPriority: "high",
		},
		{
			name:    "unknown top-level key",
			body:    map[string]any{"params": map[string]string{"branch": "main"}, "garbage": true},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, err := json.Marshal(tt.body)
			require.NoError(t, err)

			req, err := parseOptionalFireRequest(bytes.NewReader(body))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.wantParams, req.Params)
			require.Equal(t, tt.wantPriority, req.Priority)
		})
	}
}

func TestPostCreatesTrigger(t *testing.T) {
	triggerControllerTestMu.Lock()
	defer triggerControllerTestMu.Unlock()

	created := &models.Trigger{ID: uuid.New(), Alias: "new-webhook", Type: models.TriggerTypeHTTP}

	origTriggerSvcFactory := triggerServiceFactory
	defer func() { triggerServiceFactory = origTriggerSvcFactory }()

	triggerServiceFactory = func(context.Context) triggersvc.Trigger {
		return &stubTriggerService{
			createFn: func(req *triggersvc.CreateRequest) (*models.Trigger, error) {
				require.Equal(t, "new-webhook", req.Alias)
				require.Equal(t, string(models.TriggerTypeHTTP), req.Type)
				require.Equal(t, "/hooks/new-webhook", req.Configuration["path"])
				return created, nil
			},
		}
	}

	body, err := json.Marshal(map[string]any{
		"alias": "new-webhook",
		"type":  "http",
		"configuration": map[string]any{
			"path": "/hooks/new-webhook",
		},
	})
	require.NoError(t, err)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()

	err = Post(e.NewContext(req, rec))
	require.NoError(t, err)
	require.Equal(t, http.StatusCreated, rec.Code)
}

func TestPostReturnsConflictOnAliasCollision(t *testing.T) {
	triggerControllerTestMu.Lock()
	defer triggerControllerTestMu.Unlock()

	origTriggerSvcFactory := triggerServiceFactory
	defer func() { triggerServiceFactory = origTriggerSvcFactory }()
	triggerServiceFactory = func(context.Context) triggersvc.Trigger {
		return &stubTriggerService{
			createFn: func(req *triggersvc.CreateRequest) (*models.Trigger, error) {
				return nil, errors.Join(triggersvc.ErrTriggerAliasConflict, errors.New("alias already exists"))
			},
		}
	}

	body, err := json.Marshal(map[string]any{
		"alias": "new-webhook",
		"type":  "http",
		"configuration": map[string]any{
			"path": "/hooks/new-webhook",
		},
	})
	require.NoError(t, err)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()

	err = Post(e.NewContext(req, rec))
	require.Error(t, err)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusConflict, httpErr.Code)
}

func TestPostReturnsInternalServerErrorOnDBFailure(t *testing.T) {
	triggerControllerTestMu.Lock()
	defer triggerControllerTestMu.Unlock()

	origTriggerSvcFactory := triggerServiceFactory
	defer func() { triggerServiceFactory = origTriggerSvcFactory }()
	triggerServiceFactory = func(context.Context) triggersvc.Trigger {
		return &stubTriggerService{
			createFn: func(req *triggersvc.CreateRequest) (*models.Trigger, error) {
				return nil, errors.New("db down")
			},
		}
	}

	body, err := json.Marshal(map[string]any{
		"alias": "new-webhook",
		"type":  "http",
		"configuration": map[string]any{
			"path": "/hooks/new-webhook",
		},
	})
	require.NoError(t, err)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPost, "/", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()

	err = Post(e.NewContext(req, rec))
	require.Error(t, err)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusInternalServerError, httpErr.Code)
}

func TestPatchUpdatesTrigger(t *testing.T) {
	triggerControllerTestMu.Lock()
	defer triggerControllerTestMu.Unlock()

	existing := &models.Trigger{ID: uuid.New(), Alias: "existing", Type: models.TriggerTypeHTTP}

	origTriggerSvcFactory := triggerServiceFactory
	defer func() { triggerServiceFactory = origTriggerSvcFactory }()

	triggerServiceFactory = func(context.Context) triggersvc.Trigger {
		return &stubTriggerService{
			updateFn: func(id uuid.UUID, req *triggersvc.UpdateRequest) (*models.Trigger, error) {
				require.Equal(t, existing.ID, id)
				require.NotNil(t, req.Alias)
				require.Equal(t, "updated", *req.Alias)
				require.Equal(t, "/hooks/updated", req.Configuration["path"])
				return &models.Trigger{ID: existing.ID, Alias: "updated", Type: models.TriggerTypeHTTP}, nil
			},
		}
	}

	body, err := json.Marshal(map[string]any{
		"alias": "updated",
		"configuration": map[string]any{
			"path": "/hooks/updated",
		},
	})
	require.NoError(t, err)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPatch, "/", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: existing.ID.String()}})

	err = Patch(c)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
}

func TestPatchReturnsInternalServerErrorOnDBFailure(t *testing.T) {
	triggerControllerTestMu.Lock()
	defer triggerControllerTestMu.Unlock()

	existing := &models.Trigger{ID: uuid.New(), Alias: "existing", Type: models.TriggerTypeHTTP}
	origTriggerSvcFactory := triggerServiceFactory
	defer func() { triggerServiceFactory = origTriggerSvcFactory }()
	triggerServiceFactory = func(context.Context) triggersvc.Trigger {
		return &stubTriggerService{
			updateFn: func(id uuid.UUID, req *triggersvc.UpdateRequest) (*models.Trigger, error) {
				return nil, errors.New("db down")
			},
		}
	}

	body, err := json.Marshal(map[string]any{
		"alias": "updated",
		"configuration": map[string]any{
			"path": "/hooks/updated",
		},
	})
	require.NoError(t, err)

	e := echo.New()
	req := httptest.NewRequest(http.MethodPatch, "/", bytes.NewReader(body))
	req.Header.Set(echo.HeaderContentType, echo.MIMEApplicationJSON)
	rec := httptest.NewRecorder()
	c := e.NewContext(req, rec)
	c.SetPathValues(echo.PathValues{{Name: "id", Value: existing.ID.String()}})

	err = Patch(c)
	require.Error(t, err)
	httpErr, ok := err.(*echo.HTTPError)
	require.True(t, ok)
	require.Equal(t, http.StatusInternalServerError, httpErr.Code)
}
