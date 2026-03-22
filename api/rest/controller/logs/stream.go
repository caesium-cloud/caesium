package logs

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/pkg/log"
	"github.com/labstack/echo/v5"
	"go.uber.org/zap/zapcore"
)

// Stream serves an SSE endpoint that streams server log entries from the
// in-memory ring buffer.  Query parameters:
//
//	level  – minimum log level (debug, info, warn, error). Default: debug.
//	since  – sequence cursor; only entries with sequence > since are sent.
func Stream(c *echo.Context) error {
	ctx := c.Request().Context()

	minLevel, err := parseLevel(c.QueryParam("level"))
	if err != nil {
		return echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}

	var since uint64
	if s := strings.TrimSpace(c.QueryParam("since")); s != "" {
		since, err = strconv.ParseUint(s, 10, 64)
		if err != nil {
			return echo.NewHTTPError(http.StatusBadRequest, "invalid since parameter")
		}
	}

	c.Response().Header().Set(echo.HeaderContentType, "text/event-stream")
	c.Response().Header().Set(echo.HeaderCacheControl, "no-cache")
	c.Response().Header().Set(echo.HeaderConnection, "keep-alive")
	c.Response().Header().Set("X-Accel-Buffering", "no")

	flusher, ok := c.Response().(http.Flusher)
	if !ok {
		return echo.NewHTTPError(http.StatusInternalServerError, "streaming not supported")
	}

	// Initial ping.
	if _, err := fmt.Fprintf(c.Response(), ": ping\n\n"); err != nil {
		return nil
	}
	flusher.Flush()

	buf := log.Buffer()

	// Catch-up: send buffered entries that match filters.
	for _, entry := range buf.Snapshot() {
		if entry.Sequence <= since {
			continue
		}
		if !levelAtLeast(entry.Level, minLevel) {
			continue
		}
		if err := writeLogEvent(c, entry); err != nil {
			return nil
		}
		flusher.Flush()
		since = entry.Sequence
	}

	// Live stream.
	live := buf.Subscribe(ctx)

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if _, err := fmt.Fprintf(c.Response(), ": ping\n\n"); err != nil {
				return nil
			}
			flusher.Flush()
		case entry, ok := <-live:
			if !ok {
				return nil
			}
			if !levelAtLeast(entry.Level, minLevel) {
				continue
			}
			if err := writeLogEvent(c, entry); err != nil {
				return nil
			}
			flusher.Flush()
		}
	}
}

func writeLogEvent(c *echo.Context, entry log.LogEntry) error {
	data, err := json.Marshal(entry)
	if err != nil {
		return nil // skip malformed entries
	}
	if _, err := fmt.Fprintf(c.Response(), "id: %d\nevent: log\ndata: %s\n\n", entry.Sequence, data); err != nil {
		return err
	}
	return nil
}

func parseLevel(s string) (zapcore.Level, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return zapcore.DebugLevel, nil
	}
	switch s {
	case "debug":
		return zapcore.DebugLevel, nil
	case "info":
		return zapcore.InfoLevel, nil
	case "warn", "warning":
		return zapcore.WarnLevel, nil
	case "error":
		return zapcore.ErrorLevel, nil
	default:
		return zapcore.DebugLevel, fmt.Errorf("invalid level: %s", s)
	}
}

var levelOrder = map[string]int{
	"debug":  int(zapcore.DebugLevel),
	"info":   int(zapcore.InfoLevel),
	"warn":   int(zapcore.WarnLevel),
	"error":  int(zapcore.ErrorLevel),
	"dpanic": int(zapcore.DPanicLevel),
	"panic":  int(zapcore.PanicLevel),
	"fatal":  int(zapcore.FatalLevel),
}

func levelAtLeast(entryLevel string, minLevel zapcore.Level) bool {
	entryOrd, ok := levelOrder[strings.ToLower(entryLevel)]
	if !ok {
		return true // unknown levels pass through
	}
	return entryOrd >= int(minLevel)
}
