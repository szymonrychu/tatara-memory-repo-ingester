variable "name" { type = string }

resource "null_resource" "a" {
  triggers = { n = var.name }
  depends_on = [null_resource.b]
}

resource "null_resource" "b" {}

data "aws_ami" "ubuntu" {
  most_recent = true
  owners      = ["self"]
}

resource "null_resource" "c" {
  triggers = {
    ami  = data.aws_ami.ubuntu.id
    key  = local.key
    idx  = count.index
    ekey = each.key
    mod  = path.module
    ws   = terraform.workspace
  }
  depends_on = [data.aws_ami.ubuntu]
}

module "child" {
  source = "./modules/child"
  depends_on = [null_resource.b]
}

output "id" { value = null_resource.a.id }
