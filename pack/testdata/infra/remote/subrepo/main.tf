# The content of a module that Terraform FETCHES rather than reads out of the
# source tree. The test helper turns this directory into a git repository so
# the consumer stack can address it as `git::file://…`, which exercises the
# installed-module path (registry / git / http all land in the same place)
# while keeping the suite hermetic — no registry, no network.
variable "stack" {
  type = string
}

output "labels" {
  value = {
    origin = "remote"
    stack  = var.stack
  }
}
