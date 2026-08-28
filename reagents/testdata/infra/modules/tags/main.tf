variable "stack" {
  type        = string
  description = "Name of the stack these tags belong to."
}

variable "environment" {
  type    = string
  default = "test"
}

# Nested two levels below the root module, reached by a RELATIVE source.
#
# `terraform modules -json` renders this call as {"key":"inner",
# "source":"./inner"}: the key is the local call name with no parent path, and
# the source is relative to a parent directory the JSON never names, so the
# entry cannot be resolved to a real directory. The manifest that `terraform
# get` writes at .terraform/modules/modules.json carries the fully-qualified
# key and an already-resolved Dir instead. tf-discover reads the manifest for
# exactly this reason (design §6.2) — do not "simplify" this to a flat module.
module "inner" {
  source = "./inner"
  stack  = var.stack
}

output "tags" {
  value = merge(module.inner.base_tags, {
    environment = var.environment
  })
}
