package db

import (
	"fmt"
	"net"
	"strconv"

	"github.com/caesium-cloud/caesium/pkg/log"
)

// disableNagleOnDqliteNode turns Nagle's algorithm off on the local dqlite
// node's server-side TCP sockets: its listener and every connection it has
// accepted so far. On Linux an accepted socket inherits TCP_NODELAY from the
// listener, so connections accepted later are covered too.
//
// Why (#588): dqlite streams a query result as a sequence of response parts
// of one write buffer each (one OS page, 4 KiB), and it writes part N+1 only
// after part N's write has completed. The C node never sets TCP_NODELAY on the
// sockets it accepts (libuv leaves Nagle on), so the second part of a result
// is held back until the client acknowledges the first, and the Go client's
// kernel delays that acknowledgement by its delayed-ACK timer (40 ms on Linux)
// because a request/response connection has no data of its own to piggyback
// it on. Every query whose result does not fit one part therefore idled about
// 40 ms while holding a pooled connection: a run read (its task rows carry the
// log snapshot and execution descriptor) cost ~86 ms instead of ~1.4 ms, and a
// run list crossed the threshold once a job held about five runs. Under a
// sustained read mix those stalls held the read pool and the single write
// connection (reads inside write transactions stall the same way), delayed the
// scheduler's own reads, and completions fell behind arrivals.
//
// Failure to apply the option is logged, not fatal: the database still works,
// only with the stalls described above.
func disableNagleOnDqliteNode(nodeAddress string) {
	port, err := nodeAddressPort(nodeAddress)
	if err != nil {
		log.Warn("dqlite TCP_NODELAY not applied: cannot parse the node address", "address", nodeAddress, "error", err)
		return
	}
	applied, err := setNodeNoDelay(port)
	if err != nil {
		log.Warn("dqlite TCP_NODELAY not applied to every node socket", "port", port, "applied", applied, "error", err)
		return
	}
	log.Info("dqlite node sockets use TCP_NODELAY", "port", port, "sockets", applied)
}

// nodeAddressPort returns the TCP port of a dqlite node address (host:port).
func nodeAddressPort(address string) (int, error) {
	_, portText, err := net.SplitHostPort(address)
	if err != nil {
		return 0, err
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port <= 0 || port > 65535 {
		return 0, fmt.Errorf("invalid port %q", portText)
	}
	return port, nil
}
