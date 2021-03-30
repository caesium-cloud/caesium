package cluster

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/caesium-cloud/caesium/api/rest/service/private/cluster"
	"github.com/caesium-cloud/caesium/pkg/log"
)

var (
	// ErrJoinFailed is returned when a node fails to join a cluster
	ErrJoinFailed = errors.New("failed to join cluster")
)

type JoinRequest struct {
	SourceIP        string
	JoinAddress     []string
	ID              string
	Address         string
	Voter           bool
	Metadata        map[string]string
	Attempts        int
	AttemptInterval time.Duration
	TLSConfig       *tls.Config
}

// Join attempts to join the cluster at one of the addresses given in joinAddr.
// It walks through joinAddr in order, and sets the node ID and Raft address of
// the joining node as id addr respectively. It returns the endpoint successfully
// used to join the cluster.
func Join(req *JoinRequest) (string, error) {
	var (
		err error
		j   string
	)

	if req.TLSConfig == nil {
		req.TLSConfig = &tls.Config{InsecureSkipVerify: true}
	}

	for i := 0; i < req.Attempts; i++ {
		for _, a := range req.JoinAddress {
			if j, err = join(
				req.SourceIP,
				a,
				req.ID,
				req.Address,
				req.Voter,
				req.Metadata,
				req.TLSConfig,
			); err == nil {
				// Success!
				return j, nil
			}
		}

		log.Warn(
			"failed to join cluster",
			"address", req.JoinAddress,
			"sleep", req.AttemptInterval,
			"error", err,
		)

		time.Sleep(req.AttemptInterval)
	}

	log.Error(
		"failed to join cluster",
		"address", req.JoinAddress,
		"attempts", req.Attempts,
	)

	return "", ErrJoinFailed
}

func join(
	srcIP, joinAddr, id, addr string,
	voter bool,
	meta map[string]string,
	tlsConfig *tls.Config,
) (string, error) {

	if id == "" {
		return "", fmt.Errorf("node ID not set")
	}
	// The specified source IP is optional
	dialer := &net.Dialer{}
	if srcIP != "" {
		netAddr := &net.TCPAddr{
			IP:   net.ParseIP(srcIP),
			Port: 0,
		}
		dialer = &net.Dialer{LocalAddr: netAddr}
	}
	// Join using IP address, as that is what Hashicorp Raft works in.
	resv, err := net.ResolveTCPAddr("tcp", addr)
	if err != nil {
		return "", err
	}

	// Check for protocol scheme, and insert default if necessary.
	fullAddr := NormalizeAddr(fmt.Sprintf("%s/join", joinAddr))

	// Create and configure the client to connect to the other node.
	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: tlsConfig,
			Dial:            dialer.Dial,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}

	for {
		b, err := json.Marshal(cluster.JoinRequest{
			ID:       id,
			Address:  resv.String(),
			Voter:    voter,
			Metadata: meta,
		})
		if err != nil {
			return "", err
		}

		// Attempt to join.
		resp, err := client.Post(fullAddr, "application-type/json", bytes.NewReader(b))
		if err != nil {
			return "", err
		}
		defer resp.Body.Close()

		b, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return "", err
		}

		switch resp.StatusCode {
		case http.StatusOK:
			return fullAddr, nil
		case http.StatusMovedPermanently:
			fullAddr = resp.Header.Get("location")
			if fullAddr == "" {
				return "", fmt.Errorf("failed to join, invalid redirect received")
			}
			continue
		case http.StatusBadRequest:
			// One possible cause is that the target server is listening for HTTPS, but a HTTP
			// attempt was made. Switch the protocol to HTTPS, and try again. This can happen
			// when using the Disco service, since it doesn't record information about which
			// protocol a registered node is actually using.
			if strings.HasPrefix(fullAddr, "https://") {
				// It's already HTTPS, give up.
				return "", fmt.Errorf("failed to join, node returned: %s: (%s)", resp.Status, string(b))
			}

			log.Info("join via http failed, trying https")

			fullAddr = EnsureHTTPS(fullAddr)
			continue
		default:
			return "", fmt.Errorf("failed to join, node returned: %s: (%s)", resp.Status, string(b))
		}
	}
}

func NormalizeAddr(addr string) string {
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		return fmt.Sprintf("http://%s", addr)
	}
	return addr
}

func EnsureHTTPS(addr string) string {
	if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
		return fmt.Sprintf("https://%s", addr)
	}
	return strings.Replace(addr, "http://", "https://", 1)
}
