variable "canary_path" {
  type        = string
  description = <<-EOT
    Absolute path of the file this stack manages.

    It must point OUTSIDE the checked-out source tree — the materialize role
    stages a fresh clone on every run, so a file inside the tree would be
    recreated by the checkout rather than by Terraform, and its disappearance
    would not be an out-of-band change at all.
  EOT
}

resource "local_file" "canary" {
  filename        = var.canary_path
  content         = "caesium drift canary\n"
  file_permission = "0644"
}

output "canary_path" {
  value = local_file.canary.filename
}
