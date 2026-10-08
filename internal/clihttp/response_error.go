package clihttp

import (
	"fmt"
	"net/http"
	"strings"
)

// ResponseError applies the common CLI status and body-read error precedence.
// A zero status means request construction or transport failed, so that error
// remains unwrapped. Other statuses below 400 are successful unless the body
// could not be read completely.
func ResponseError(operation string, status int, body []byte, readErr error) error {
	if status == 0 {
		return readErr
	}
	if status >= http.StatusBadRequest {
		if readErr != nil {
			return fmt.Errorf("%s failed (%d): %s (reading response: %w)", operation, status, strings.TrimSpace(string(body)), readErr)
		}
		return fmt.Errorf("%s failed (%d): %s", operation, status, strings.TrimSpace(string(body)))
	}
	if readErr != nil {
		if status >= http.StatusOK && status < http.StatusMultipleChoices {
			return fmt.Errorf("%s response: accepted (%d), but response body incomplete; the operation may already have taken effect, verify before retrying: %w", operation, status, readErr)
		}
		return fmt.Errorf("reading %s response: %w", operation, readErr)
	}
	return nil
}
