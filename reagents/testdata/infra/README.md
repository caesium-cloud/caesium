# Hermetic fake infra repo

The fixture every reagent test and infra integration scenario drives (design §9).
It is deliberately boring: three stacks, two shared modules, the `local`
backend, and the `null`/`random` providers, so it plans and applies with no
cloud credentials and no network beyond the provider registry.

```
stacks/network    exports vpc_id, vpcId, db-url (and a sensitive admin_token)
stacks/account    ordered after network
stacks/app-web    consumes those names through TF_VAR_vpc_id / TF_VAR_vpcId / TF_VAR_db-url
modules/vpc       shared; used by network and app-web
modules/tags      shared; calls modules/tags/inner by a RELATIVE source
fail-closed/dynamic-source   a module source that is a variable
remote/subrepo    module content the helper turns into a git repository
remote/consumer   a stack whose module source is git::file:// (not a local path)
drift/canary      the one stack whose provider READS the real world
stacks.yaml       inter-stack order for multi-root discover
```

Three things here are load-bearing and easy to break by "tidying":

- **`modules/tags/inner` is reached by the relative source `./inner`, two
  levels below the root module.** `terraform modules -json` renders it as
  `{"key":"inner","source":"./inner"}` — a local call name with no parent path
  and a source relative to a parent the JSON does not name, so it cannot be
  resolved to a directory. The `.terraform/modules/modules.json` manifest
  carries `{"Key":"tags.inner","Dir":"…/modules/tags/inner"}` instead. This is
  the whole reason `tf-discover` reads the manifest (design §6.2), and editing
  a file in `inner` must move the fingerprint of every stack that uses `tags`.
- **`fail-closed/dynamic-source` lives outside `stacks/`** so a multi-root
  discover over `stacks/` never trips over it. It exists only for the test that
  a module `source` which cannot reduce to a constant makes `terraform get`
  exit non-zero, and therefore makes discover fail closed with no fingerprint.
  A stack that cannot be resolved must make the run red, never green-with-skips.
- **`network`'s `admin_token` is `sensitive = true`.** It must never reach a
  task output row or an API response.
- **`remote/consumer` is the only stack whose module Terraform must *fetch*.**
  Registry, git and http sources all get installed into `TF_DATA_DIR`, and
  discover relocates that to a per-run scratch directory so the source mount can
  stay read-only — which makes the manifest's `Dir` for such a module an
  absolute, per-run path. It has to be read from and must never be digested;
  the module's stable identity is its source and version. Every other stack here
  uses local relative sources, so without this one that whole path — the shape
  of nearly every real Terraform repo — is untested. `remote/consumer/main.tf`
  is rendered from `main.tf.tmpl` by the test helper because `git::file://`
  needs an absolute path that no committed file can carry, and `remote/` sits
  outside `stacks/` so multi-root discover never scans it.

- **`drift/canary` is the only stack whose refresh can detect anything.**
  `null` and `random` are state-only providers: their `ReadResource` hands back
  whatever state already says, so `plan -refresh-only` returns exit 0 for the
  three stacks under `stacks/` no matter how their state is edited — including
  after a `terraform state rm`. `local_file` is different: its read checks that
  the file still exists and still hashes to what state recorded, and drops the
  resource when it does not. Deleting that file is therefore a real out-of-band
  change, and it is what makes the drift job (design §6.6) testable at all. Its
  `canary_path` must point outside the checked-out tree, because the materialize
  role stages a fresh clone on every run.

`.terraform.lock.hcl` is committed and locked for `linux_amd64` and
`linux_arm64` so the warm step's mirror key is stable across CI runners.
Regenerate it with `terraform providers lock -platform=linux_amd64
-platform=linux_arm64` from a stack directory.
