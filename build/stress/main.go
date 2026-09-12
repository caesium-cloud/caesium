// Resource-stress is a bounded integration fixture, not a production workload.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path"
	"runtime"
	"strings"
	"syscall"
	"time"
)

func main() {
	os.Exit(run())
}

func run() int {
	memoryMiB := flag.Int("memory-mib", 16, "MiB of resident memory to allocate (1..1024)")
	hold := flag.Duration("hold", 2*time.Second, "time to retain the allocation (0..5m)")
	waitFile := flag.String("wait-file", "", "wait for this file before allocating")
	waitTimeout := flag.Duration("wait-timeout", time.Minute, "maximum wait for the release file (0..5m)")
	flag.Parse()
	if flag.NArg() != 0 || *memoryMiB < 1 || *memoryMiB > 1024 || *hold < 0 || *hold > 5*time.Minute || *waitTimeout <= 0 || *waitTimeout > 5*time.Minute {
		fmt.Fprintln(os.Stderr, "invalid arguments: memory must be 1..1024 MiB, hold 0..5m, wait-timeout >0..5m, with no positional arguments")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *waitFile != "" {
		fmt.Printf("waiting for %s\n", *waitFile)
		if err := awaitFile(ctx, *waitFile, *waitTimeout); err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 1
		}
	}
	// Report the limit the memory cgroup carries at the moment the workload
	// starts. A harness that injects a limit after the container started can
	// verify it from this line on any engine: Podman 4.9 applies a live update
	// to the OCI runtime without rewriting the spec its inspect reports, and
	// reading it here costs the container under test no extra exec session.
	fmt.Printf("cgroup memory limit %s\n", cgroupMemoryLimit())
	// Touch every page, rather than relying on a virtual reservation. Keep the
	// allocation reachable through the hold so GC cannot release the workload.
	memory := make([]byte, *memoryMiB*1024*1024)
	for i := 0; i < len(memory); i += 4096 {
		memory[i] = 1
	}
	fmt.Printf("allocated %d MiB\n", *memoryMiB)
	timer := time.NewTimer(*hold)
	defer timer.Stop()
	select {
	case <-ctx.Done():
	case <-timer.C:
	}
	runtime.KeepAlive(memory)
	fmt.Println("completed")
	return 0
}

func awaitFile(ctx context.Context, path string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(20 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect release file: %w", err)
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("wait for release file: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

// cgroupMemoryLimit reports the byte limit on this process's own memory cgroup,
// or "unavailable" when neither hierarchy exposes one.
func cgroupMemoryLimit() string {
	for _, name := range cgroupMemoryLimitFiles() {
		if value, err := os.ReadFile(name); err == nil {
			return strings.TrimSpace(string(value))
		}
	}
	return "unavailable"
}

// cgroupMemoryLimitFiles resolves the cgroup through /proc/self/cgroup so the
// reading holds under both a private and a host cgroup namespace, and prefers
// cgroup v2 over v1.
func cgroupMemoryLimitFiles() []string {
	var v2, v1 string
	if content, err := os.ReadFile("/proc/self/cgroup"); err == nil {
		for _, line := range strings.Split(string(content), "\n") {
			fields := strings.SplitN(line, ":", 3)
			if len(fields) != 3 {
				continue
			}
			if fields[0] == "0" && fields[1] == "" {
				v2 = fields[2]
				continue
			}
			for _, controller := range strings.Split(fields[1], ",") {
				if controller == "memory" {
					v1 = fields[2]
				}
			}
		}
	}
	return []string{
		path.Join("/sys/fs/cgroup", v2, "memory.max"),
		"/sys/fs/cgroup/memory.max",
		path.Join("/sys/fs/cgroup/memory", v1, "memory.limit_in_bytes"),
		"/sys/fs/cgroup/memory/memory.limit_in_bytes",
	}
}
