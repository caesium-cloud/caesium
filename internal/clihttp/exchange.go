// Package clihttp contains HTTP transport shared by CLI callers. Each caller
// retains its status, error-label and output policy.
package clihttp

import (
	"context"
	"io"
	"net/http"
)

// Exchange sends a request with the supplied client and headers and reads its
// response to completion. A body read error retains the partial data and status
// so the caller can choose its diagnostic precedence. Response bodies are closed
// even when reading fails.
func Exchange(ctx context.Context, client *http.Client, method, url string, body io.Reader, headers http.Header) (data []byte, status int, err error) {
	req, err := http.NewRequestWithContext(ctx, method, url, body)
	if err != nil {
		return nil, 0, err
	}
	req.Header = headers.Clone()
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	data, err = io.ReadAll(resp.Body)
	return data, resp.StatusCode, err
}
