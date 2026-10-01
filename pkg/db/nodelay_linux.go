//go:build linux

package db

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

// setNodeNoDelay sets TCP_NODELAY on every TCP socket this process holds whose
// LOCAL port is port. For the dqlite node's port that is exactly its listener
// and the server side of every connection it accepted; client sockets dialed by
// the Go driver have an ephemeral local port (and already run with
// TCP_NODELAY, which Go sets on every TCP connection). It returns how many
// sockets were set.
func setNodeNoDelay(port int) (int, error) {
	return forEachLocalPortSocket(port, func(fd int) error {
		return unix.SetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY, 1)
	})
}

// nodeSocketNoDelay reports, for every TCP socket this process holds on local
// port port, whether TCP_NODELAY is set, keyed by file descriptor. The
// listener is reported under the key listener.
func nodeSocketNoDelay(port int) (listener *bool, accepted map[int]bool, err error) {
	accepted = map[int]bool{}
	_, err = forEachLocalPortSocket(port, func(fd int) error {
		on, err := unix.GetsockoptInt(fd, unix.IPPROTO_TCP, unix.TCP_NODELAY)
		if err != nil {
			return err
		}
		listening, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_ACCEPTCONN)
		if err != nil {
			return err
		}
		if listening == 1 {
			value := on != 0
			listener = &value
			return nil
		}
		accepted[fd] = on != 0
		return nil
	})
	return listener, accepted, err
}

// forEachLocalPortSocket calls fn for every IPv4/IPv6 stream socket among this
// process's open file descriptors whose local port is port, and returns how
// many calls succeeded. The descriptors belong to the dqlite C library, which
// owns their lifetime; a descriptor that is closed (or even reused) between
// the listing and the call is skipped or, at worst, gets a harmless option on
// another stream socket, so no lock with the library is needed.
func forEachLocalPortSocket(port int, fn func(fd int) error) (int, error) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, fmt.Errorf("list open descriptors: %w", err)
	}
	var (
		visited int
		errs    []error
	)
	for _, entry := range entries {
		fd, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		target, err := os.Readlink("/proc/self/fd/" + entry.Name())
		if err != nil || !strings.HasPrefix(target, "socket:") {
			continue
		}
		if typ, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TYPE); err != nil || typ != unix.SOCK_STREAM {
			continue
		}
		local, err := unix.Getsockname(fd)
		if err != nil {
			continue
		}
		switch addr := local.(type) {
		case *unix.SockaddrInet4:
			if addr.Port != port {
				continue
			}
		case *unix.SockaddrInet6:
			if addr.Port != port {
				continue
			}
		default:
			continue
		}
		if err := fn(fd); err != nil {
			if errors.Is(err, unix.EBADF) || errors.Is(err, unix.ENOTSOCK) {
				continue // closed since it was listed
			}
			errs = append(errs, fmt.Errorf("fd %d: %w", fd, err))
			continue
		}
		visited++
	}
	return visited, errors.Join(errs...)
}
