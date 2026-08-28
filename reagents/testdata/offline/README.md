# Provider-free root module

A Terraform root module that `init`s, `plan`s and applies with **no network at
all**: its only resource is `terraform_data`, which lives in Terraform's
built-in `terraform.io/builtin/terraform` provider and therefore needs no
download, and its backend is `local`.

That is the whole point. `reagents/testdata/infra` — the fixture the integration
lane drives — requires the `null`/`random` providers, so any test that runs
Terraform against it needs either the network or a populated mirror. The
reagents' own unit tests are hermetic (`just reagents-test` is verified under
`--network none`), so `tf-runner`'s phases are exercised against this module
instead: a real `terraform init`, a real plan artifact, a real apply, and real
`terraform output -json`.

`token` is `sensitive = true` and must never reach an emitted output row.
`structured` is an object, which exists to prove a non-scalar output is rendered
as compact JSON rather than dropped by `pkg/task.ParseOutput`'s scalar filter.

The one thing this module cannot exercise is *detected* drift: `terraform_data`
is a state-only resource, so its refresh is a no-op and `plan -refresh-only`
can never report a difference for it. The clean half of the drift contract is
tested here; the drifted half needs a resource whose read consults the real
world, which is what `reagents/testdata/infra/drift/canary` is for.
