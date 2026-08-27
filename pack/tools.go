//go:build tools

// This file exists only to pin dependencies that a later role binary needs but
// no current source file imports, so `go mod tidy` keeps them. Terraform
// knowledge in the pack is built on terraform-exec + terraform-json (design
// §6.7); resolving both up front means the streams that add tf-discover and
// tf-runner never race each other in go.sum.
//
// The `tools` build tag is never enabled, so nothing here is compiled into any
// image.
package pack

import (
	_ "github.com/hashicorp/terraform-exec/tfexec"
	_ "github.com/hashicorp/terraform-json"
)
