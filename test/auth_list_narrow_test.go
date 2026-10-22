//go:build integration

package test

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/google/uuid"
)

// TestAuthListNamespaceNarrow drives the optional ?namespace= view filter of
// GET /v1/jobs, /v1/triggers and /v1/incidents (identity-and-access W7-δ, F2)
// against the live server: a marketing-confined viewer sees its own rows with
// ?namespace=marketing, an empty 200 (never a 403) for ?namespace=finance, an
// unchanged result without the parameter, and a 400 for a malformed value;
// the cluster-wide admin narrows to either namespace. It needs no policy file:
// any namespace string can own a job on the api-key lane.
func (s *IntegrationTestSuite) TestAuthListNamespaceNarrow() {
	s.requireAuthLane()
	f := s.newNamespacedRoutesFixture("narrow")
	defer s.closeNamespacedRoutesFixture(f)
	viewer := s.mintNamespacedKey("viewer", "marketing").Plaintext

	type row struct {
		ID        string `json:"id"`
		Alias     string `json:"alias"`
		TriggerID string `json:"trigger_id"`
	}
	list := func(token, path string) []row {
		s.T().Helper()
		status, body := s.requestWithKey(http.MethodGet, path, token, nil)
		s.Require().Equal(http.StatusOK, status, "%s: %s", path, body)
		rows := []row{}
		s.Require().NoError(json.Unmarshal([]byte(body), &rows), body)
		return rows
	}
	has := func(rows []row, id string) bool {
		for _, r := range rows {
			if r.ID == id {
				return true
			}
		}
		return false
	}

	jobs := list(s.authAPIKey, "/v1/jobs?limit=500")
	var mktTrigger, finTrigger string
	for _, j := range jobs {
		switch j.ID {
		case f.mktJob.ID:
			mktTrigger = j.TriggerID
		case f.finJob.ID:
			finTrigger = j.TriggerID
		}
	}
	s.Require().NotEmpty(mktTrigger)
	s.Require().NotEmpty(finTrigger)

	// Jobs.
	s.True(has(list(viewer, "/v1/jobs?namespace=marketing"), f.mktJob.ID), "the confined viewer lists its own job")
	s.False(has(list(viewer, "/v1/jobs?namespace=marketing"), f.finJob.ID))
	s.Empty(list(viewer, "/v1/jobs?namespace=finance"), "a namespace outside the grants is an empty 200, not a 403")
	unfiltered := list(viewer, "/v1/jobs")
	s.True(has(unfiltered, f.mktJob.ID))
	s.False(has(unfiltered, f.finJob.ID))
	s.Equal(len(unfiltered), len(list(viewer, "/v1/jobs?namespace=*")), `"*" does not narrow or widen`)
	adminFin := list(s.authAPIKey, "/v1/jobs?namespace=finance&limit=500")
	s.True(has(adminFin, f.finJob.ID))
	s.False(has(adminFin, f.mktJob.ID))
	status, _ := s.requestWithKey(http.MethodGet, "/v1/jobs?namespace=Not_A_Label", s.authAPIKey, nil)
	s.Equal(http.StatusBadRequest, status)

	// Triggers belong to a namespace through their live jobs.
	s.True(has(list(viewer, "/v1/triggers?namespace=marketing"), mktTrigger))
	s.False(has(list(viewer, "/v1/triggers?namespace=marketing"), finTrigger))
	s.Empty(list(viewer, "/v1/triggers?namespace=finance"), "a namespace outside the grants is an empty 200")
	status, _ = s.requestWithKey(http.MethodGet, "/v1/triggers?namespace=Not_A_Label", s.authAPIKey, nil)
	s.Equal(http.StatusBadRequest, status)
	adminTriggers := list(s.authAPIKey, "/v1/triggers?namespace=finance&limit=500")
	s.True(has(adminTriggers, finTrigger))
	s.False(has(adminTriggers, mktTrigger))

	// Incidents. Seeded closed in each namespace (a precondition the catalog
	// produces; remediation never picks closed rows up), then read through the
	// live feed: the selected namespace's id is present, the other's absent.
	now := time.Now().UTC()
	seedIncident := func(job *jobSummary, namespace string) string {
		id := uuid.NewString()
		s.catalogExec(f, 1, `INSERT INTO incidents (id, namespace, job_id, class, status, dedupe_key, opened_at, closed_at, created_at, updated_at) VALUES (?, ?, ?, 'w7_narrow_check', 'closed', ?, ?, ?, ?, ?)`,
			id, namespace, job.ID, "w7/"+id, now, now, now, now)
		return id
	}
	mktIncident, finIncident := seedIncident(f.mktJob, "marketing"), seedIncident(f.finJob, "finance")
	f.undo = append(f.undo, func() {
		s.catalogExec(f, -1, `DELETE FROM incidents WHERE id IN (?, ?)`, mktIncident, finIncident)
	})
	both := s.mintNamespacedKey("viewer", "marketing", "finance").Plaintext

	incidents := func(token, query string) (int, map[string]bool, int) {
		s.T().Helper()
		code, body := s.requestWithKey(http.MethodGet, "/v1/incidents"+query, token, nil)
		if code != http.StatusOK {
			return code, nil, 0
		}
		var feed struct {
			Incidents []struct {
				ID string `json:"id"`
			} `json:"incidents"`
			Total int `json:"total"`
		}
		s.Require().NoError(json.Unmarshal([]byte(body), &feed), body)
		ids := map[string]bool{}
		for _, in := range feed.Incidents {
			ids[in.ID] = true
		}
		s.Equal(len(feed.Incidents), feed.Total, "total counts only the narrowed rows")
		return code, ids, feed.Total
	}
	_, ids, _ := incidents(viewer, "?namespace=marketing&limit=200")
	s.True(ids[mktIncident], "the confined viewer lists its own incident")
	s.False(ids[finIncident])
	code, ids, total := incidents(viewer, "?namespace=finance")
	s.Equal(http.StatusOK, code, "a namespace outside the grants is an empty 200")
	s.Zero(total)
	s.Empty(ids)
	_, ids, _ = incidents(both, "?namespace=finance&limit=200")
	s.True(ids[finIncident], "a principal who reads both narrows to the selected one")
	s.False(ids[mktIncident])
	_, ids, _ = incidents(both, "?namespace=marketing&limit=200")
	s.True(ids[mktIncident])
	s.False(ids[finIncident])
	_, ids, _ = incidents(both, "?limit=200")
	s.True(ids[mktIncident] && ids[finIncident], "no parameter keeps both")
	_, ids, _ = incidents(s.authAPIKey, "?namespace=finance&limit=200")
	s.True(ids[finIncident])
	s.False(ids[mktIncident])
	code, _, _ = incidents(s.authAPIKey, "?namespace=Not_A_Label")
	s.Equal(http.StatusBadRequest, code)
}
