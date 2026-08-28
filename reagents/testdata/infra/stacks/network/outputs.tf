output "vpc_id" {
  value = module.vpc.vpc_id
}

output "tags" {
  value = module.tags.tags
}

# A sensitive output must never appear in a task output row or an API response
# (design §6.4, test scenario §9.9).
output "admin_token" {
  value     = random_password.admin.result
  sensitive = true
}
