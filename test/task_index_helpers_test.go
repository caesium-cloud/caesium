//go:build integration

package test

import "fmt"

func (s *IntegrationTestSuite) taskNamesByID(jobID string) map[string]string {
	s.T().Helper()
	var tasks []struct {
		ID   string `json:"ID"`
		Name string `json:"Name"`
	}
	s.getJSON(fmt.Sprintf("/v1/jobs/%s/tasks", jobID), &tasks)
	nameByID := make(map[string]string, len(tasks))
	for _, task := range tasks {
		nameByID[task.ID] = task.Name
	}
	return nameByID
}
