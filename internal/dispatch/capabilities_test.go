package dispatch

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/caesium-cloud/caesium/internal/models"
	"github.com/caesium-cloud/caesium/internal/run"
	"github.com/stretchr/testify/require"
)

// capableSubmitter is a submitter that advertises instance identity, mirroring
// what *worker.Worker does once it is accepting dispatched tasks.
type capableSubmitter struct {
	fakeSubmitter
}

func (c *capableSubmitter) Capabilities() []string {
	return []string{CapabilityInstanceIdentity}
}

func getCapabilities(t *testing.T, h *Handler) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/internal/capabilities", nil)
	req.Header.Set("Authorization", "Bearer "+testToken)
	w := httptest.NewRecorder()
	h.HandleCapabilities(w, req)
	return w
}

// TestHandleCapabilities_AdvertisesTheWorkersCapabilities pins the advertisement
// contract an owner's rolling-deploy gate reads: the node reports its protocol
// version and the capability set of the worker actually wired into it.
func TestHandleCapabilities_AdvertisesTheWorkersCapabilities(t *testing.T) {
	_, _, h := setupHandler(t)
	h = h.WithWorkerSubmitter(&capableSubmitter{})

	w := getCapabilities(t, h)
	require.Equal(t, http.StatusOK, w.Code)

	var got CapabilitiesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, ownerNodeAddr, got.NodeID)
	require.Equal(t, InternalProtocolVersion, got.ProtocolVersion)
	require.True(t, got.Supports(CapabilityInstanceIdentity),
		"a node with an instance-capable worker must advertise it")
}

// TestHandleCapabilities_NodeWithNoWorkerAdvertisesNothing: a node that cannot
// execute anything must not be picked for a fan-out instance.  It would reject
// the dispatch anyway, and saying so up front keeps the owner from burning a
// tick on it.
func TestHandleCapabilities_NodeWithNoWorkerAdvertisesNothing(t *testing.T) {
	_, _, h := setupHandler(t)

	w := getCapabilities(t, h)
	require.Equal(t, http.StatusOK, w.Code)

	var got CapabilitiesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.False(t, got.Supports(CapabilityInstanceIdentity))
	require.Empty(t, got.Capabilities)
}

// TestHandleCapabilities_RequiresTheBearerToken keeps the probe on the same
// footing as the traffic it gates.
func TestHandleCapabilities_RequiresTheBearerToken(t *testing.T) {
	_, _, h := setupHandler(t)
	h = h.WithWorkerSubmitter(&capableSubmitter{})

	req := httptest.NewRequest(http.MethodGet, "/internal/capabilities", nil)
	w := httptest.NewRecorder()
	h.HandleCapabilities(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestGetCapabilities_ReadsALegacyPeerAsIncapable: an older build serves no such
// route.  The probe must report that as "supports nothing", never as a
// transient error the caller might optimistically ignore.
func TestGetCapabilities_ReadsALegacyPeerAsIncapable(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(legacy.Close)

	caps, err := GetCapabilities(t.Context(), legacy.URL+"/internal/capabilities", testToken)
	require.Error(t, err, "a 404 from a pre-capability build must be an error, not a silent yes")
	require.False(t, caps.Supports(CapabilityInstanceIdentity))
}

// TestHandleDispatch_RejectsInstanceBlindDispatchForAnExpandedGroup pins the
// receiving half of the rolling-upgrade guard.
//
// Only an owner that predates instance-addressed dispatch omits TaskRunID, and
// for a group that is already expanded its catalog id names N rows.  Every write
// downstream resolves that identity through loadTaskRunByIDOrUnique, so
// accepting the request either strands a claim inside a transaction behind an
// opaque 409 or drives a legacy group-wide write.  It is rejected up front, with
// a reason code that names the mismatch, and nothing is claimed.
func TestHandleDispatch_RejectsInstanceBlindDispatchForAnExpandedGroup(t *testing.T) {
	store, _, h := setupHandler(t)
	sub := &fakeSubmitter{}
	h = h.WithWorkerSubmitter(sub)

	runID, taskID, _ := seedFannedRun(t, store.DB(), store)

	req := DispatchRequest{
		RunID:           runID,
		TaskID:          taskID,
		OwnerGeneration: 1,
		Attempt:         1,
		WorkerNode:      ownerNodeAddr,
		OwnerBaseURL:    "http://10.0.0.1:8080",
		Deadline:        time.Now().Add(5 * time.Minute),
	}
	w := postJSON(t, h.HandleDispatch, req)
	require.Equal(t, http.StatusConflict, w.Code)

	var body ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Equal(t, ReasonAmbiguousTask, body.Code,
		"the rejection must be typed so an operator can tell it from a lost claim race")

	require.Empty(t, sub.accepted, "an ambiguous dispatch must never reach the worker")

	var rows []models.TaskRun
	require.NoError(t, store.DB().Where("job_run_id = ? AND task_id = ?", runID, taskID).Find(&rows).Error)
	require.Len(t, rows, 3)
	for i := range rows {
		require.Equal(t, string(run.TaskStatusPending), rows[i].Status,
			"a rejected ambiguous dispatch must not claim any sibling")
		require.Empty(t, rows[i].ClaimedBy)
	}
}

// TestHandleDispatch_UnfannedTaskWithoutTaskRunIDStillWorks is the control: the
// ambiguity guard must not break the legitimate legacy path, where a catalog id
// names exactly one row.
func TestHandleDispatch_UnfannedTaskWithoutTaskRunIDStillWorks(t *testing.T) {
	store, _, h := setupHandler(t)
	sub := &fakeSubmitter{}
	h = h.WithWorkerSubmitter(sub)

	runID, taskID := seedPendingTaskRun(t, store)

	req := DispatchRequest{
		RunID:           runID,
		TaskID:          taskID,
		OwnerGeneration: 1,
		Attempt:         1,
		WorkerNode:      ownerNodeAddr,
		OwnerBaseURL:    "http://10.0.0.1:8080",
		Deadline:        time.Now().Add(5 * time.Minute),
	}
	w := postJSON(t, h.HandleDispatch, req)
	require.Equal(t, http.StatusAccepted, w.Code,
		"an unfanned task is unambiguous by its catalog id and must still dispatch")
	require.Len(t, sub.accepted, 1)
}

// TestGetCapabilities_DistinguishesUnreachableFromLegacy: the dispatch loop
// benches a peer it cannot reach but must NOT bench one that merely answers 404
// — that peer is a healthy older node and still takes unfanned work.
func TestGetCapabilities_DistinguishesUnreachableFromLegacy(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(http.NotFound))
	t.Cleanup(legacy.Close)

	_, err := GetCapabilities(t.Context(), legacy.URL+"/internal/capabilities", testToken)
	require.Error(t, err)
	require.False(t, errors.Is(err, ErrPeerUnreachable),
		"a 404 is an answer, not a transport failure; benching that peer would starve unfanned dispatch")

	dead := httptest.NewServer(http.HandlerFunc(http.NotFound))
	deadURL := dead.URL
	dead.Close()

	_, err = GetCapabilities(t.Context(), deadURL+"/internal/capabilities", testToken)
	require.Error(t, err)
	require.True(t, errors.Is(err, ErrPeerUnreachable),
		"a node that cannot be dialled must be reported as unreachable so it gets benched")
}
