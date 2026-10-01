package system

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/labstack/echo/v5"
	"github.com/stretchr/testify/require"
)

// staleID is dqlite's bootstrap ID, above 2^53: it must round-trip exactly.
const staleID uint64 = 3297041220608546238

func serveRemove(t *testing.T, path string, remove func(context.Context, uint64) (dqlite.MemberRemoval, error)) *httptest.ResponseRecorder {
	t.Helper()
	previous := removeMember
	removeMember = remove
	t.Cleanup(func() { removeMember = previous })

	e := echo.New()
	e.DELETE("/v1/system/nodes/:id", RemoveNode)
	req := httptest.NewRequestWithContext(context.Background(), http.MethodDelete, path, nil)
	rec := httptest.NewRecorder()
	e.ServeHTTP(rec, req)
	return rec
}

func TestRemoveNodeRejectsInvalidIDsBeforeTouchingTheCluster(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-1", "18446744073709551616", "1.5"} {
		t.Run(raw, func(t *testing.T) {
			rec := serveRemove(t, "/v1/system/nodes/"+raw, func(context.Context, uint64) (dqlite.MemberRemoval, error) {
				t.Fatal("an invalid ID reached the cluster")
				return dqlite.MemberRemoval{}, nil
			})
			require.Equal(t, http.StatusBadRequest, rec.Code)
			var body RefusalResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, "refused", body.Status)
			require.Equal(t, ReasonInvalidID, body.Reason)
			require.Equal(t, []string{ReasonInvalidID}, body.Reasons)
		})
	}
}

func TestRemoveNodeMapsEveryRefusalToAStatusAndReason(t *testing.T) {
	cases := []struct {
		reason dqlite.RemovalReason
		status int
	}{
		{dqlite.RemovalNotAMember, http.StatusNotFound},
		{dqlite.RemovalNoLeader, http.StatusServiceUnavailable},
		{dqlite.RemovalNotClustered, http.StatusConflict},
		{dqlite.RemovalLocalNode, http.StatusConflict},
		{dqlite.RemovalLeader, http.StatusConflict},
		{dqlite.RemovalVoter, http.StatusConflict},
		{dqlite.RemovalInsufficientVoters, http.StatusConflict},
		{dqlite.RemovalReachable, http.StatusConflict},
		{dqlite.RemovalVotersUnreachable, http.StatusConflict},
		{dqlite.RemovalConfigurationChange, http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(string(tc.reason), func(t *testing.T) {
			reachable := false
			rec := serveRemove(t, "/v1/system/nodes/3297041220608546238", func(_ context.Context, id uint64) (dqlite.MemberRemoval, error) {
				require.Equal(t, staleID, id)
				return dqlite.MemberRemoval{}, &dqlite.MemberRemovalRefusal{
					ID: id, Reason: tc.reason, Reasons: []dqlite.RemovalReason{tc.reason, dqlite.RemovalInsufficientVoters},
					Detail: "because", Retryable: tc.reason == dqlite.RemovalConfigurationChange,
					Member: &dqlite.MemberRecord{ID: id, Address: "10.244.2.5:9001", Role: "spare", Reachable: &reachable},
					Leader: &dqlite.MemberRecord{ID: 7, Address: "10.244.1.9:9001", Role: "voter", Leader: true},
				}
			})
			require.Equal(t, tc.status, rec.Code, rec.Body.String())
			var body RefusalResponse
			require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
			require.Equal(t, "refused", body.Status)
			require.Equal(t, string(tc.reason), body.Reason)
			require.Equal(t, []string{string(tc.reason), string(dqlite.RemovalInsufficientVoters)}, body.Reasons)
			require.Equal(t, "3297041220608546238", body.ID)
			require.Equal(t, "because", body.Message)
			require.Equal(t, tc.reason == dqlite.RemovalConfigurationChange, body.Retryable)
			require.NotNil(t, body.Member)
			require.Equal(t, "3297041220608546238", body.Member.ID)
			require.NotNil(t, body.Member.Reachable)
			require.False(t, *body.Member.Reachable)
			require.NotNil(t, body.Leader)
			require.True(t, body.Leader.Leader)
		})
	}
}

func TestRemoveNodeReportsTheRemovedMemberAndTheRemainingConfiguration(t *testing.T) {
	rec := serveRemove(t, "/v1/system/nodes/3297041220608546238", func(_ context.Context, id uint64) (dqlite.MemberRemoval, error) {
		return dqlite.MemberRemoval{
			Removed: dqlite.MemberRecord{ID: id, Address: "10.244.2.5:9001", Role: "spare"},
			Leader:  dqlite.MemberRecord{ID: 7, Address: "10.244.1.9:9001", Role: "voter", Leader: true},
			Members: []dqlite.MemberRecord{
				{ID: 7, Address: "10.244.1.9:9001", Role: "voter", Leader: true},
				{ID: 8, Address: "10.244.2.9:9001", Role: "voter"},
				{ID: 18446744073709551615, Address: "10.244.3.9:9001", Role: "voter"},
			},
		}, nil
	})
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var body RemovalResponse
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &body))
	require.Equal(t, "removed", body.Status)
	require.Equal(t, MemberView{ID: "3297041220608546238", Address: "10.244.2.5:9001", Role: "spare"}, body.Removed)
	require.Equal(t, "7", body.Leader.ID)
	require.Len(t, body.Members, 3)
	require.Equal(t, "18446744073709551615", body.Members[2].ID, "IDs are strings so no digit is lost")
}

func TestRemoveNodeHidesUnexpectedFailures(t *testing.T) {
	rec := serveRemove(t, "/v1/system/nodes/42", func(context.Context, uint64) (dqlite.MemberRemoval, error) {
		return dqlite.MemberRemoval{}, errors.New("disk on fire")
	})
	require.Equal(t, http.StatusInternalServerError, rec.Code)
	require.NotContains(t, rec.Body.String(), "disk on fire")

	rec = serveRemove(t, "/v1/system/nodes/42", func(context.Context, uint64) (dqlite.MemberRemoval, error) {
		return dqlite.MemberRemoval{}, context.DeadlineExceeded
	})
	require.Equal(t, http.StatusServiceUnavailable, rec.Code)
}
