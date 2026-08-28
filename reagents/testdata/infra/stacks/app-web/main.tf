# app-web consumes network's vpc_id through TF_VAR_vpc_id — the cross-unit
# wiring that must still bust app-web's cache when network's output changes,
# even under `cache.chain: values` (test scenario §9.4). The variable has no
# default on purpose: a stack that silently plans against an empty vpc_id would
# be a green run that deployed the wrong thing.
variable "vpc_id" {
  type        = string
  description = "VPC id exported by the network stack."
}

module "tags" {
  source = "../../modules/tags"
  stack  = "app-web"
}

module "vpc" {
  source = "../../modules/vpc"
  name   = "app-web"
}

# templatefile() reads a file that is not Terraform configuration and does not
# live at the top level of the module. Editing templates/userdata.tftpl changes
# what this stack deploys while every *.tf file stays byte-identical, so a
# fingerprint that covered only the configuration globs would report the stack
# unchanged and plan/apply would cache-hit — a green run that deployed none of
# the edit.
locals {
  userdata = templatefile("${path.module}/templates/userdata.tftpl", {
    vpc_id = var.vpc_id
  })
}

resource "null_resource" "app_web" {
  triggers = {
    vpc_id   = var.vpc_id
    replica  = var.replica_count
    userdata = local.userdata
  }
}

output "endpoint" {
  value = "https://app-web.${var.vpc_id}.example.internal"
}

output "tags" {
  value = module.tags.tags
}
