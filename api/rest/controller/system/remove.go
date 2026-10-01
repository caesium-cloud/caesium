package system

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/caesium-cloud/caesium/pkg/dqlite"
	"github.com/labstack/echo/v5"
)

// Member removal (H1, #582 follow-up): `DELETE /v1/system/nodes/:id` removes the
// stale raft entry a disk-loss replacement leaves behind. The safety checks live
// in pkg/dqlite (RemoveStaleMember); this handler only maps their outcome onto
// HTTP. Every refusal is a JSON body with a machine-readable reason, so the CLI
// and scripts never have to parse prose:
//
//	200 removed
//	400 invalid_id
//	404 not_a_member
//	409 local_node, leader, voter, insufficient_voters, reachable,
//	    voters_unreachable, configuration_change_in_progress, not_clustered
//	503 no_leader
//
// `retryable` says whether the same request may succeed later without anyone
// changing anything (a configuration change or election in flight, or a lost
// voter that role adjustment has yet to demote).

// removeMember is a variable so the HTTP mapping can be tested without a
// cluster.
var removeMember = dqlite.RemoveStaleMember

// MemberView is one raft member in a removal answer. The ID is a decimal string
// because dqlite node IDs are random 64-bit values.
type MemberView struct {
	ID        string `json:"id"`
	Address   string `json:"address"`
	Role      string `json:"role"`
	Leader    bool   `json:"leader"`
	Reachable *bool  `json:"reachable,omitempty"`
}

// RemovalResponse is the 200 answer.
type RemovalResponse struct {
	Status  string       `json:"status"`
	Removed MemberView   `json:"removed"`
	Leader  MemberView   `json:"leader"`
	Members []MemberView `json:"members"`
}

// RefusalResponse is every non-200 answer the removal itself produces.
type RefusalResponse struct {
	Status    string      `json:"status"`
	ID        string      `json:"id"`
	Reason    string      `json:"reason"`
	Reasons   []string    `json:"reasons"`
	Message   string      `json:"message"`
	Retryable bool        `json:"retryable"`
	Member    *MemberView `json:"member,omitempty"`
	Leader    *MemberView `json:"leader,omitempty"`
}

// ReasonInvalidID is the one refusal decided here rather than in pkg/dqlite.
const ReasonInvalidID = "invalid_id"

// RemoveNode removes a stale, non-voting, unreachable dqlite member by node ID
// through the current leader.
func RemoveNode(c *echo.Context) error {
	raw := strings.TrimSpace(c.Param("id"))
	id, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || id == 0 {
		return c.JSON(http.StatusBadRequest, RefusalResponse{
			Status: "refused", ID: raw, Reason: ReasonInvalidID, Reasons: []string{ReasonInvalidID},
			Message: fmt.Sprintf("%q is not a dqlite node ID; use the decimal id listed by GET /v1/system/nodes", raw),
		})
	}

	result, err := removeMember(c.Request().Context(), id)
	if err != nil {
		var refusal *dqlite.MemberRemovalRefusal
		if errors.As(err, &refusal) {
			return c.JSON(refusalStatus(refusal.Reason), refusalResponse(refusal))
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return echo.NewHTTPError(http.StatusServiceUnavailable, "member removal did not complete").Wrap(err)
		}
		return echo.NewHTTPError(http.StatusInternalServerError, "internal server error").Wrap(err)
	}

	members := make([]MemberView, 0, len(result.Members))
	for _, m := range result.Members {
		members = append(members, memberView(m))
	}
	return c.JSON(http.StatusOK, RemovalResponse{
		Status:  "removed",
		Removed: memberView(result.Removed),
		Leader:  memberView(result.Leader),
		Members: members,
	})
}

func refusalStatus(reason dqlite.RemovalReason) int {
	switch reason {
	case dqlite.RemovalNotAMember:
		return http.StatusNotFound
	case dqlite.RemovalNoLeader:
		return http.StatusServiceUnavailable
	default:
		return http.StatusConflict
	}
}

func refusalResponse(r *dqlite.MemberRemovalRefusal) RefusalResponse {
	reasons := make([]string, 0, len(r.Reasons))
	for _, reason := range r.Reasons {
		reasons = append(reasons, string(reason))
	}
	if len(reasons) == 0 {
		reasons = append(reasons, string(r.Reason))
	}
	resp := RefusalResponse{
		Status:    "refused",
		ID:        strconv.FormatUint(r.ID, 10),
		Reason:    string(r.Reason),
		Reasons:   reasons,
		Message:   r.Detail,
		Retryable: r.Retryable,
	}
	if r.Member != nil {
		v := memberView(*r.Member)
		resp.Member = &v
	}
	if r.Leader != nil {
		v := memberView(*r.Leader)
		resp.Leader = &v
	}
	return resp
}

func memberView(m dqlite.MemberRecord) MemberView {
	return MemberView{ID: strconv.FormatUint(m.ID, 10), Address: m.Address, Role: m.Role,
		Leader: m.Leader, Reachable: m.Reachable}
}
