//go:build integration

package test

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type datasetListResponse struct {
	Datasets []datasetStateResponse `json:"datasets"`
	Total    int64                  `json:"total"`
}

type datasetStateResponse struct {
	Namespace string `json:"namespace,omitempty"`
	Name      string `json:"name"`
	Watermark string `json:"watermark"`
	Status    string `json:"status"`
}

type datasetAdvanceResponse struct {
	Outcome string               `json:"outcome"`
	State   datasetStateResponse `json:"state"`
}

type datasetDetailResponse struct {
	State datasetStateResponse `json:"state"`
	SLO   *datasetSLOResponse  `json:"slo,omitempty"`
}

type datasetSLOResponse struct {
	ExpectedEvery string `json:"expected_every,omitempty"`
}

type datasetDerivationsResponse struct {
	Derivations []json.RawMessage `json:"derivations"`
	Total       int64             `json:"total"`
}

func (s *IntegrationTestSuite) TestDatasetRESTAndCLIListSurfacesManualAdvance() {
	declaredName := fmt.Sprintf("declared_%d", time.Now().UnixNano())
	alias := fmt.Sprintf("integration-dataset-declared-%d", time.Now().UnixNano())
	dir := s.writeJobManifest(datasetDeclaredSourceManifest(alias, declaredName))
	defer os.RemoveAll(dir)
	s.runCLI("job", "apply", "--path", dir, "--server", s.caesiumURL)

	var declared datasetDetailResponse
	s.getJSON("/v1/datasets/_/"+declaredName, &declared)
	s.Equal(declaredName, declared.State.Name)
	s.Equal("unknown", declared.State.Status)
	s.Require().NotNil(declared.SLO)
	s.Equal("1h", declared.SLO.ExpectedEvery)

	name := fmt.Sprintf("manual_%d", time.Now().UnixNano())
	watermark := fmt.Sprintf("%d", time.Now().UnixNano())
	payload := fmt.Sprintf(`{"watermark":%q}`, watermark)

	resp, err := s.doRequest(http.MethodPost, s.caesiumURL+"/v1/datasets/_/"+name+"/advance", strings.NewReader(payload))
	s.Require().NoError(err)
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	s.Require().NoError(err)
	s.Require().Equal(http.StatusOK, resp.StatusCode, string(body))

	var advanced datasetAdvanceResponse
	s.Require().NoError(json.Unmarshal(body, &advanced))
	s.Equal("advanced", advanced.Outcome)
	s.Equal("", advanced.State.Namespace)
	s.Equal(name, advanced.State.Name)
	s.Equal(watermark, advanced.State.Watermark)
	s.Equal("unknown", advanced.State.Status)

	var detail datasetDetailResponse
	s.getJSON("/v1/datasets/_/"+name, &detail)
	s.Equal(name, detail.State.Name)
	s.Equal(watermark, detail.State.Watermark)
	s.Equal("unknown", detail.State.Status)

	var listed datasetListResponse
	s.getJSON("/v1/datasets?status=unknown", &listed)
	state, ok := findDatasetState(listed.Datasets, "", name)
	s.Require().True(ok, "advanced dataset %q not found in list response: %+v", name, listed.Datasets)
	s.Equal(watermark, state.Watermark)
	s.Equal("unknown", state.Status)

	var derivations datasetDerivationsResponse
	s.getJSON("/v1/datasets/_/"+name+"/derivations", &derivations)
	s.Len(derivations.Derivations, 0)
	s.Equal(int64(0), derivations.Total)

	stdout, err := s.runCLIStdout("dataset", "list", "--json", "--server", s.caesiumURL)
	s.Require().NoError(err)
	s.Require().True(json.Valid([]byte(stdout)), "caesium dataset list stdout was not clean JSON:\n%s", stdout)

	var cliList datasetListResponse
	s.Require().NoError(json.Unmarshal([]byte(stdout), &cliList))
	_, ok = findDatasetState(cliList.Datasets, "", name)
	s.Require().True(ok, "advanced dataset %q not found in CLI list response: %+v", name, cliList.Datasets)

	statusOut, err := s.runCLIStdout("dataset", "status", name, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err)
	s.Require().True(json.Valid([]byte(statusOut)), "caesium dataset status stdout was not clean JSON:\n%s", statusOut)

	var cliStatus datasetDetailResponse
	s.Require().NoError(json.Unmarshal([]byte(statusOut), &cliStatus))
	s.Equal(name, cliStatus.State.Name)
	s.Equal(watermark, cliStatus.State.Watermark)

	cliName := fmt.Sprintf("manual_cli_%d", time.Now().UnixNano())
	cliWatermark := fmt.Sprintf("%d", time.Now().UnixNano())
	advanceOut, err := s.runCLIStdout("dataset", "advance", cliName, "--watermark", cliWatermark, "--server", s.caesiumURL)
	s.Require().NoError(err)
	s.Require().True(json.Valid([]byte(advanceOut)), "caesium dataset advance stdout was not clean JSON:\n%s", advanceOut)

	var cliAdvanced datasetAdvanceResponse
	s.Require().NoError(json.Unmarshal([]byte(advanceOut), &cliAdvanced))
	s.Equal("advanced", cliAdvanced.Outcome)
	s.Equal(cliName, cliAdvanced.State.Name)
	s.Equal(cliWatermark, cliAdvanced.State.Watermark)

	// A dataset name that contains dots (e.g. "raw.vendor_x") must round-trip as
	// a single name in the empty namespace, not be split into namespace.name.
	dottedName := fmt.Sprintf("raw.vendor_%d", time.Now().UnixNano())
	dottedWatermark := fmt.Sprintf("%d", time.Now().UnixNano())
	dottedAdvanceOut, err := s.runCLIStdout("dataset", "advance", dottedName, "--watermark", dottedWatermark, "--server", s.caesiumURL)
	s.Require().NoError(err)
	var dottedAdvanced datasetAdvanceResponse
	s.Require().NoError(json.Unmarshal([]byte(dottedAdvanceOut), &dottedAdvanced))
	s.Equal(dottedName, dottedAdvanced.State.Name)
	s.Equal("", dottedAdvanced.State.Namespace)

	dottedStatusOut, err := s.runCLIStdout("dataset", "status", dottedName, "--json", "--server", s.caesiumURL)
	s.Require().NoError(err)
	var dottedStatus datasetDetailResponse
	s.Require().NoError(json.Unmarshal([]byte(dottedStatusOut), &dottedStatus))
	s.Equal(dottedName, dottedStatus.State.Name)
	s.Equal(dottedWatermark, dottedStatus.State.Watermark)
}

func findDatasetState(rows []datasetStateResponse, namespace, name string) (datasetStateResponse, bool) {
	for _, row := range rows {
		if row.Namespace == namespace && row.Name == name {
			return row, true
		}
	}
	return datasetStateResponse{}, false
}

func datasetDeclaredSourceManifest(alias, datasetName string) string {
	return fmt.Sprintf(`apiVersion: v1
kind: Job
metadata:
  alias: %s
  datasets:
    sources:
      - name: %s
        expectedEvery: 1h
        external: true
trigger: { type: cron, configuration: { cron: "0 2 * * *" } }
steps:
  - name: noop
    image: alpine:3.23
    command: ["sh","-c","echo ok"]
`, alias, datasetName)
}
