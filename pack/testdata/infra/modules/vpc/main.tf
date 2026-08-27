terraform {
  required_providers {
    random = {
      source  = "hashicorp/random"
      version = "~> 3.6"
    }
  }
}

variable "name" {
  type        = string
  description = "Logical name of the network this VPC backs."
}

variable "cidr_block" {
  type    = string
  default = "10.0.0.0/16"
}

resource "random_id" "vpc" {
  byte_length = 4

  keepers = {
    name = var.name
  }
}

output "vpc_id" {
  value = "vpc-${random_id.vpc.hex}"
}

output "cidr_block" {
  value = var.cidr_block
}
