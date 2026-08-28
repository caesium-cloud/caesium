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

output "account_tags" {
  # Deliberately NOT "tags": network exports one too, and a stack importing
  # from both (the diamond form) would collide on TF_VAR_tags. Two upstream
  # stacks exporting the same name is a real ambiguity the runner refuses; the
  # fixture disambiguates so the diamond itself stays exercisable.
  value = module.tags.tags
}
