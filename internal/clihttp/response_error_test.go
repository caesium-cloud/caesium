package clihttp

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestResponseErrorPreservesCLIErrorPrecedence(t *testing.T) {
	sentinel := errors.New("response interrupted")
	for _, tc := range []struct {
		name      string
		status    int
		body      []byte
		readErr   error
		want      string
		wantErr   error
		unwrapped bool
	}{
		{name: "empty successful response", status: 204},
		{name: "redirect remains successful", status: 302, body: []byte("redirect body")},
		{name: "raw successful body remains caller data", status: 200, body: []byte("not JSON")},
		{name: "complete JSON error body", status: 500, body: []byte(" \n{\"error\":\"unavailable\"} \n"), want: "backfill list failed (500): {\"error\":\"unavailable\"}"},
		{name: "partial error body keeps status primary", status: 503, body: []byte(" partial JSON "), readErr: sentinel, want: "backfill list failed (503): partial JSON (reading response: response interrupted)", wantErr: sentinel},
		{name: "successful body read error", status: 200, readErr: sentinel, want: "backfill list accepted (200), but response body incomplete; the operation may already have taken effect, verify before retrying: response interrupted", wantErr: sentinel},
		{name: "mutation accepted with incomplete response", status: 202, readErr: sentinel, want: "backfill list accepted (202), but response body incomplete; the operation may already have taken effect, verify before retrying: response interrupted", wantErr: sentinel},
		{name: "all sub-400 statuses use successful read label", status: 399, readErr: sentinel, want: "reading backfill list response: response interrupted", wantErr: sentinel},
		{name: "request or transport error stays unwrapped", status: 0, readErr: sentinel, want: "response interrupted", wantErr: sentinel, unwrapped: true},
		{name: "zero status without error stays empty success", status: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := ResponseError("backfill list", tc.status, tc.body, tc.readErr)
			if tc.want == "" {
				require.NoError(t, got)
				return
			}
			require.Error(t, got)
			require.EqualError(t, got, tc.want)
			if tc.wantErr != nil {
				if tc.unwrapped {
					require.Same(t, tc.wantErr, got)
				} else {
					require.ErrorIs(t, got, tc.wantErr)
				}
			}
		})
	}
}
