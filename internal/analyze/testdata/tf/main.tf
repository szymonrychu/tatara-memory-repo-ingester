variable "name" { type = string }

resource "null_resource" "a" {
  triggers = { n = var.name }
  depends_on = [null_resource.b]
}

resource "null_resource" "b" {}

module "child" {
  source = "./modules/child"
  depends_on = [null_resource.b]
}

output "id" { value = null_resource.a.id }
