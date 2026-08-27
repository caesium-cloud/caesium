// The Caesium pack is a deliberately separate Go module. The pack images are
// Terraform-aware (spec §6.7 — terraform-exec + terraform-json), and Caesium
// core must stay unaware of HCL, so those dependencies never enter the root
// module's go.mod/go.sum. Nothing here may import
// github.com/caesium-cloud/caesium/... : the contract between the two is the
// stdout marker protocol, not a Go API.
module github.com/caesium-cloud/caesium/pack

go 1.25.6

require (
	github.com/hashicorp/terraform-exec v0.25.3
	github.com/hashicorp/terraform-json v0.28.0
)

require (
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/hashicorp/go-version v1.9.0 // indirect
	github.com/zclconf/go-cty v1.18.1 // indirect
	golang.org/x/text v0.39.0 // indirect
)
