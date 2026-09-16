package dispatch

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/caesium-cloud/caesium/pkg/task"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"gorm.io/datatypes"
)

func TestHandleCompleteApplicationRejectionPreservesClaimAndDetails(t *testing.T) {
	for _, memory := range []bool{false, true} {
		for _, status := range []string{"succeeded", "cached"} {
			t.Run(status+map[bool]string{false: "/sql", true: "/memory"}[memory], func(t *testing.T) {
				store, ls, h := setupHandler(t)
				runID, taskID := seedPendingTaskRun(t, store)
				var producer models.Task
				require.NoError(t, store.DB().First(&producer, "id = ?", taskID).Error)
				consumer := models.Task{ID: uuid.New(), JobID: producer.JobID, AtomID: producer.AtomID, Name: "process", FanOutConfig: datatypes.JSON([]byte(`{"from":"step1","onEmpty":"fail"}`))}
				require.NoError(t, store.DB().Create(&consumer).Error)
				require.NoError(t, store.DB().Create(&models.TaskEdge{ID: uuid.New(), JobID: producer.JobID, FromTaskID: taskID, ToTaskID: consumer.ID}).Error)
				require.NoError(t, store.DB().Create(&models.TaskRun{ID: uuid.New(), JobRunID: runID, TaskID: consumer.ID, AtomID: consumer.AtomID, Status: "pending", OutstandingPredecessors: 1}).Error)
				_, err := ls.AcquireLease(t.Context(), runID, ownerNodeAddr, time.Hour)
				require.NoError(t, err)
				require.NoError(t, store.ClaimTaskForDispatch(runID, taskID, ownerNodeAddr, 1, time.Hour, true))
				var row models.TaskRun
				require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", runID, taskID).First(&row).Error)
				if memory {
					manager := run.NewOwnerManager(store, run.CheckpointConfig{})
					_, err = manager.Recover(runID, 1)
					require.NoError(t, err)
					require.NoError(t, store.ClaimTaskForDispatch(runID, taskID, ownerNodeAddr, 1, time.Hour, true))
					manager.MarkDispatched(runID, taskID, ownerNodeAddr, 1, time.Now().Add(time.Hour).UnixMilli())
					h.WithOwnerManager(manager)
				}
				server := httptest.NewServer(http.HandlerFunc(h.HandleComplete))
				defer server.Close()
				req := CompleteRequest{RunID: runID, TaskID: taskID, TaskRunID: row.ID, OwnerGeneration: 1, WorkerNode: ownerNodeAddr, Status: status, Result: "success", Partitions: []task.Partition{{Key: "a", DependsOn: []string{"b"}}, {Key: "b", DependsOn: []string{"a"}}}}
				w := postJSON(t, h.HandleComplete, req)
				if memory {
					require.Equal(t, http.StatusOK, w.Code)
					var accepted CompleteResponse
					require.NoError(t, json.Unmarshal(w.Body.Bytes(), &accepted))
					require.True(t, accepted.Accepted)
					require.NoError(t, store.DB().First(&row, "id = ?", row.ID).Error)
					require.Equal(t, "failed", row.Status)
					require.Contains(t, row.Error, "cycle")
					require.Positive(t, row.TerminalSequence)
					return
				}
				require.Equal(t, http.StatusUnprocessableEntity, w.Code)
				var response ErrorResponse
				require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
				require.Equal(t, ReasonCompletionApplicationRejected, response.Code)
				require.Contains(t, response.Message, "cycle")
				_, err = PostComplete(t.Context(), server.URL, testToken, req)
				require.ErrorIs(t, err, ErrCompletionApplicationRejected)
				require.NotErrorIs(t, err, ErrOwnerRejected)
				require.Contains(t, err.Error(), response.Message)
				require.NoError(t, store.DB().First(&row, "id = ?", row.ID).Error)
				require.Equal(t, "running", row.Status)
				require.Zero(t, row.TerminalSequence, "rejected success must roll back its terminal mutation")
				req.Status = "failed"
				req.Error = response.Message
				req.Partitions = nil
				accepted, err := PostComplete(t.Context(), server.URL, testToken, req)
				require.NoError(t, err)
				require.True(t, accepted.Accepted)
				require.NoError(t, store.DB().First(&row, "id = ?", row.ID).Error)
				require.Equal(t, "failed", row.Status)
				require.Contains(t, row.Error, "cycle")
				require.Positive(t, row.TerminalSequence)
			})
		}
	}
}

func TestPostCompleteUnknownApplicationResponsesAreNotPermanentValidation(t *testing.T) {
	for _, test := range []struct {
		status        int
		code, message string
	}{
		{http.StatusUnprocessableEntity, "", "invalid result"},
		{http.StatusUnprocessableEntity, "unknown", "invalid result"},
		{http.StatusInternalServerError, ReasonCompletionApplicationRejected, "storage unavailable"},
		{http.StatusConflict, ReasonStaleGeneration, "stale generation"},
	} {
		t.Run(test.code+http.StatusText(test.status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeJSON(w, test.status, ErrorResponse{Code: test.code, Message: test.message})
			}))
			defer server.Close()
			_, err := PostComplete(t.Context(), server.URL, "token", CompleteRequest{})
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrCompletionApplicationRejected)
			require.Contains(t, err.Error(), test.message)
			if test.status == http.StatusConflict {
				require.ErrorIs(t, err, ErrOwnerRejected)
			} else {
				require.NotErrorIs(t, err, ErrOwnerRejected)
			}
		})
	}
}

func TestCompletionUnexpectedApplyFaultIsServerError(t *testing.T) {
	w := httptest.NewRecorder()
	writeCompletionApplyError(w, fmt.Errorf("database connection lost"))
	require.Equal(t, http.StatusInternalServerError, w.Code)
	var response ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &response))
	require.Equal(t, ReasonCompletionApplyFailed, response.Code)
	require.Equal(t, "database connection lost", response.Message)
}

func TestPostCompleteMalformedApplicationCodeDoesNotAuthorizeValidation(t *testing.T) {
	for _, status := range []int{http.StatusUnprocessableEntity, http.StatusConflict} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			code := ReasonCompletionApplicationRejected
			if status == http.StatusConflict {
				code = ReasonTaskNotRunning
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(status)
				_, err := fmt.Fprintf(w, `{"code":%q,"message":42}`, code)
				require.NoError(t, err)
			}))
			defer server.Close()
			_, err := PostComplete(t.Context(), server.URL, "token", CompleteRequest{})
			require.Error(t, err)
			require.NotErrorIs(t, err, ErrCompletionApplicationRejected)
			if status == http.StatusConflict {
				require.ErrorIs(t, err, ErrOwnerRejected)
			}
		})
	}
}
