# account is ordered after network by stacks.yaml, but does not consume any of
# network's outputs. It is the stack that must stay `cached` when app-web
# changes — the load-bearing half of test scenario §9.2.
variable "vpc_id" {
  type    = string
  default = ""
}

module "tags" {
  source = "../../modules/tags"
  stack  = "account"
}

resource "null_resource" "account" {
  triggers = {
    stack = "account"
  }
}

output "account_id" {
  value = "acct-000000000001"
}

output "tags" {
  value = module.tags.tags
}
