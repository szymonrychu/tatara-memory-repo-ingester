variable "shared" {}

resource "null_resource" "r" {
  triggers = { v = var.shared }
}

output "o" {
  value = null_resource.r.id
}
