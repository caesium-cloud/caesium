// Package streaming keeps long-lived HTTP responses within a bounded write
// deadline without expiring healthy streams at the server's initial deadline.
package streaming

import (
	"errors"
	"io"
	"net/http"
	"time"
)

// WriteTimeout bounds each stream write, including its subsequent flush.
const WriteTimeout = 30 * time.Second

type writer struct{ response http.ResponseWriter }

// Writer renews the write deadline before each chunk. ResponseController unwraps
// Echo's response to reach the underlying connection. Unsupported deadlines
// (for example, in-memory test recorders) still permit ordinary writes.
func Writer(response http.ResponseWriter) io.Writer { return writer{response} }

func (w writer) Write(p []byte) (int, error) {
	err := http.NewResponseController(w.response).SetWriteDeadline(time.Now().Add(WriteTimeout))
	if err != nil && !errors.Is(err, http.ErrNotSupported) {
		return 0, err
	}
	return w.response.Write(p)
}
