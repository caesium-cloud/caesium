terraform {
  required_version = ">= 1.10.0"

  # BYO state, and no provider requirements at all: terraform_data is built in,
  # so `terraform init` here needs no registry and no mirror.
  backend "local" {
    path = "terraform.tfstate"
  }
}

variable "greeting" {
  type    = string
  default = "hello"
}

resource "terraform_data" "canary" {
  input = var.greeting
}

output "greeting" {
  value = terraform_data.canary.output
}

# Must never appear in an emitted output row, the tasks API, or `caesium why`
# (design §6.4, test scenario §9.9).
output "token" {
  value     = "s3cr3t-canary"
  sensitive = true
}

# A non-scalar output: pkg/task.ParseOutput keeps only scalars, so this has to
# be rendered as compact JSON or the key is silently dropped.
output "structured" {
  value = { a = 1, b = "two" }
}
