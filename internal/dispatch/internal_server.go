package dispatch

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"time"

	"github.com/caesium-cloud/caesium/pkg/log"
)

// InternalServer hosts the run-owner coordination endpoints
// (/internal/dispatch, /internal/complete) on a dedicated, mutually
// authenticated TLS listener — separate from the public API server, which stays
// plain HTTP behind the operator's own proxy.  Node-to-node coordination
// traffic therefore never touches the public surface and every peer is
// authenticated by client certificate at the TLS layer.
type InternalServer struct {
	srv *http.Server
}

// NewInternalServer builds the internal mTLS server for handler, bound to addr
// (e.g. ":8443") with the supplied server TLS config (from ServerTLSConfig).
func NewInternalServer(handler *Handler, addr string, tlsConfig *tls.Config) *InternalServer {
	mux := http.NewServeMux()
	mux.HandleFunc("/internal/dispatch", handler.HandleDispatch)
	mux.HandleFunc("/internal/complete", handler.HandleComplete)
	return &InternalServer{
		srv: &http.Server{
			Addr:      addr,
			Handler:   mux,
			TLSConfig: tlsConfig,
			// Bound every phase of a connection so a slow or stuck peer can't
			// tie up the internal listener (slowloris-style resource exhaustion).
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       15 * time.Second,
			WriteTimeout:      15 * time.Second,
			IdleTimeout:       120 * time.Second,
		},
	}
}

// Run serves until Shutdown is called. It blocks; run it in a goroutine. The
// certificate comes from srv.TLSConfig.Certificates, so ServeTLS is called with
// empty file arguments.
func (s *InternalServer) Run(ctx context.Context) error {
	var lc net.ListenConfig
	ln, err := lc.Listen(ctx, "tcp", s.srv.Addr)
	if err != nil {
		return err
	}

	log.Info("internal mTLS listener started", "addr", s.srv.Addr)
	if err := s.srv.ServeTLS(ln, "", ""); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully drains the internal mTLS listener.
func (s *InternalServer) Shutdown(ctx context.Context) error {
	if s == nil || s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}
