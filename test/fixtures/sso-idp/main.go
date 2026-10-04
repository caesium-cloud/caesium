//go:build integration

// The sso-idp executable is a test fixture, never a production authenticator.
package main

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"time"
)

const clientID = "coverage-caesium"

func randomID() string {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		panic("fixture entropy unavailable")
	}
	return base64.RawURLEncoding.EncodeToString(b)
}
func origin(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "http" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("fixture requires a plain task HTTP origin")
	}
	u.Path = ""
	return u, nil
}
func writeJSON(w http.ResponseWriter, value any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(value)
}
func emit(value any) { _ = json.NewEncoder(os.Stdout).Encode(value) }
func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "use serve or journey")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(os.Args[2:])
	case "journey":
		err = runJourney(os.Args[2:])
	default:
		err = errors.New("unknown fixture command")
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "SSO fixture failed; see sanitized phase event")
		os.Exit(1)
	}
}
func serve(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", ":8090", "fixture HTTP bind")
	issuer := fs.String("issuer", "", "task IdP origin")
	sp := fs.String("sp-base", "", "task Caesium origin")
	metadata := fs.String("metadata-file", "", "public metadata output")
	if err := fs.Parse(args); err != nil {
		return err
	}
	idpURL, err := origin(*issuer)
	if err != nil {
		return err
	}
	spURL, err := origin(*sp)
	if err != nil {
		return err
	}
	secret := os.Getenv("SSO_FIXTURE_CLIENT_SECRET")
	if secret == "" {
		return errors.New("fixture client secret required")
	}
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return err
	}
	o := newOIDC(idpURL.String(), spURL.String(), secret, key)
	s, err := newSAML(idpURL.String(), spURL.String(), key)
	if err != nil {
		return err
	}
	if *metadata == "" {
		return errors.New("metadata file required")
	}
	encoded, err := s.metadata()
	if err != nil {
		return err
	}
	// Metadata is public. A fresh collector-owned volume avoids symlink/race reuse.
	f, err := os.OpenFile(*metadata, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644)
	if err != nil {
		return err
	}
	if _, err = f.Write(encoded); err != nil {
		_ = f.Close()
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ready", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]any{"ready": true, "version": 1, "issuer": o.issuer, "sp_base": s.spBase})
	})
	mux.HandleFunc("/.well-known/openid-configuration", o.discovery)
	mux.HandleFunc("/keys", o.keys)
	mux.HandleFunc("/authorize", o.authorize)
	mux.HandleFunc("/token", o.token)
	mux.HandleFunc("/saml/metadata", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/samlmetadata+xml")
		_, _ = w.Write(encoded)
	})
	mux.HandleFunc("/saml/sso", s.sso)
	mux.HandleFunc("/stats", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, o.stats()) })
	server := &http.Server{Addr: *listen, Handler: mux, ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 15 * time.Second, IdleTimeout: 15 * time.Second}
	err = server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}
func client() *http.Client {
	return &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error { return http.ErrUseLastResponse }}
}
func modeAllowed(mode string, allowed ...string) bool {
	if mode == "" {
		return true
	}
	for _, candidate := range allowed {
		if mode == candidate {
			return true
		}
	}
	return false
}
