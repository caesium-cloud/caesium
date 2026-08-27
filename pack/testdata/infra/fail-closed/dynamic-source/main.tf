# A module source that is an input variable. Terraform rejects this at install
# time:
#
#   Error: Unknown module source
#     source = var.module_source
#   Only literal values and const variables can be evaluated during init.
#
# so `terraform get` exits non-zero and tf-discover fails closed with no
# fingerprint (design §6.2, test scenario §9.10). Counterintuitively a `locals`
# value holding a literal DOES resolve, while an input variable does not — even
# with a literal default, and even when passed explicitly with -var. This stack
# regression-guards the upstream behaviour the design depends on, so it lives
# outside stacks/ where a multi-root discover will never scan it.
variable "module_source" {
  type    = string
  default = "../../modules/tags"
}

module "tags" {
  source = var.module_source
  stack  = "dynamic"
}

output "tags" {
  value = module.tags.tags
}
