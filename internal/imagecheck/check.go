// Package imagecheck verifies container image availability locally.
package imagecheck

import (
	"context"
	"strings"

	"github.com/docker/docker/client"
)

// Result describes whether a container image is available locally.
type Result struct {
	Image     string
	Available bool
	Error     error
}

// Check inspects the local Docker daemon for each image without pulling.
func Check(ctx context.Context, images []string) []Result {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		results := make([]Result, len(images))
		for i, img := range images {
			results[i] = Result{Image: img, Error: err}
		}
		return results
	}
	defer cli.Close()

	results := make([]Result, len(images))
	for i, img := range images {
		_, _, inspectErr := cli.ImageInspectWithRaw(ctx, img)
		if inspectErr == nil {
			results[i] = Result{Image: img, Available: true}
		} else if isNotFound(inspectErr) {
			results[i] = Result{Image: img, Available: false}
		} else {
			results[i] = Result{Image: img, Error: inspectErr}
		}
	}
	return results
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "No such image")
}
