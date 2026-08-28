module "tags" {
  source = "../../modules/tags"
  stack  = "network"
}

module "vpc" {
  source     = "../../modules/vpc"
  name       = "network"
  cidr_block = var.cidr_block
}

resource "null_resource" "network" {
  triggers = {
    vpc_id = module.vpc.vpc_id
  }
}

resource "random_password" "admin" {
  length  = 24
  special = false
}
