//go:build integration

package test

import "os"

// resourceStressImage uses the exact architecture/commit-tagged fixture loaded
// by CI, never an unrelated registry tag. Local recipes build this tag first.
func resourceStressImage() string {
	if image := os.Getenv("CAESIUM_RESOURCE_STRESS_IMAGE"); image != "" {
		return image
	}
	return "caesiumcloud/resource-stress:latest"
}
