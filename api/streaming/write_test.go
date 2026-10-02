package streaming

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/labstack/echo/v5"
)

func TestStreamOutlivesInitialServerWriteDeadline(t *testing.T) {
	e := echo.New()
	e.GET("/stream", func(c *echo.Context) error {
		c.Response().Header().Set("Content-Type", "text/event-stream")
		w := Writer(c.Response())
		if _, err := fmt.Fprint(w, ": start\n\n"); err != nil {
			return err
		}
		c.Response().(http.Flusher).Flush()
		time.Sleep(160 * time.Millisecond)
		_, err := fmt.Fprint(w, ": done\n\n")
		return err
	})
	srv := httptest.NewUnstartedServer(e)
	srv.Config.WriteTimeout = 50 * time.Millisecond
	srv.Start()
	defer srv.Close()
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, srv.URL+"/stream", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("stream truncated after initial deadline: %v", err)
	}
	if string(body) != ": start\n\n: done\n\n" {
		t.Fatalf("missing stream chunks: %q", body)
	}
}

func TestWriterSupportsResponsesWithoutDeadlines(t *testing.T) {
	response := httptest.NewRecorder()
	if _, err := io.Copy(Writer(response), strings.NewReader(": ping\n\n")); err != nil {
		t.Fatal(err)
	}
	if response.Body.String() != ": ping\n\n" {
		t.Fatalf("unexpected response: %q", response.Body.String())
	}
}
