variable "stack" {
  type        = string
  description = "Name of the stack these tags belong to."
}

output "base_tags" {
  value = {
    managed_by = "caesium"
    stack      = var.stack
  }
}
