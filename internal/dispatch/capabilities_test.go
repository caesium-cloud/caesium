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

// testCapability is a placeholder capability string used only to exercise the
// generic CapabilityAdvertiser plumbing.  No production type advertises a real
// capability today (the last one, instance_identity, was removed in #358 once
// every deployed node was guaranteed to speak instance-addressed dispatch).
const testCapability = "test_capability"

// capableSubmitter is a submitter that advertises a capability, mirroring the
// shape a future *worker.Worker feature could take once it implements
// CapabilityAdvertiser.
type capableSubmitter struct {
	fakeSubmitter
}

func (c *capableSubmitter) Capabilities() []string {
	return []string{testCapability}
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
// contract: the node reports its protocol version and the capability set of
// whatever CapabilityAdvertiser is wired into it as the submitter.
func TestHandleCapabilities_AdvertisesTheWorkersCapabilities(t *testing.T) {
	_, _, h := setupHandler(t)
	h = h.WithWorkerSubmitter(&capableSubmitter{})

	w := getCapabilities(t, h)
	require.Equal(t, http.StatusOK, w.Code)

	var got CapabilitiesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, ownerNodeAddr, got.NodeID)
	require.Equal(t, InternalProtocolVersion, got.ProtocolVersion)
	require.True(t, got.Supports(testCapability),
		"a node whose submitter advertises a capability must report it")
}

// TestHandleCapabilities_NodeWithNoWorkerAdvertisesNothing: a node with no
// worker attached — and, today, every real *worker.Worker, since it advertises
// no capability — reports an empty capability set alongside its protocol
// version.
func TestHandleCapabilities_NodeWithNoWorkerAdvertisesNothing(t *testing.T) {
	_, _, h := setupHandler(t)

	w := getCapabilities(t, h)
	require.Equal(t, http.StatusOK, w.Code)

	var got CapabilitiesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.False(t, got.Supports(testCapability))
	require.Empty(t, got.Capabilities)
}

// TestHandleCapabilities_RequiresTheBearerToken pins the bearer-token
// requirement on GET /internal/capabilities itself: nothing gates dispatch on
// the probe's answer any more (#358), but the endpoint is still guarded by
// the same auth check as every other internal endpoint.
func TestHandleCapabilities_RequiresTheBearerToken(t *testing.T) {
	_, _, h := setupHandler(t)
	h = h.WithWorkerSubmitter(&capableSubmitter{})

	req := httptest.NewRequest(http.MethodGet, "/internal/capabilities", nil)
	w := httptest.NewRecorder()
	h.HandleCapabilities(w, req)
	require.Equal(t, http.StatusUnauthorized, w.Code)
}

// TestGetCapabilities_ReadsALegacyPeerAsIncapable pins GetCapabilities's own
// error taxonomy on the kept endpoint: an older build serving no such route
// (404) must be reported as an error ("supports nothing"), never as a
// zero-value success. GetCapabilities has no production caller today (the
// dispatch loop's capability-gated use of it was removed in #358); this pins
// the exported helper's contract for whatever calls it next.
func TestGetCapabilities_ReadsALegacyPeerAsIncapable(t *testing.T) {
	legacy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	t.Cleanup(legacy.Close)

	caps, err := GetCapabilities(t.Context(), legacy.URL+"/internal/capabilities", testToken)
	require.Error(t, err, "a 404 from a pre-capability build must be an error, not a silent yes")
	require.False(t, caps.Supports(testCapability))
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

// TestGetCapabilities_DistinguishesUnreachableFromLegacy pins another piece of
// GetCapabilities's error taxonomy for the kept endpoint: a 404 (a peer that
// answered "no such route") must be reported as a plain error, distinct from
// ErrPeerUnreachable, which wraps only a transport failure. No production
// caller of GetCapabilities tells them apart today — the dispatch loop's own
// bench-on-capability-probe was removed along with the gate in #358 — but the
// exported contract stays correct for whatever calls GetCapabilities next.
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
