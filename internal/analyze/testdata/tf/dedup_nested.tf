# Test fixture for finding 1 (edge dedup) and finding 2 (nested block traversal).

variable "bucket_name" { type = string }

# Finding 1: var.bucket_name appears in two attributes of the same resource.
# Without dedup, two identical var_ref edges are emitted.
resource "aws_s3_bucket" "main" {
  bucket = var.bucket_name
  tags = {
    Name = var.bucket_name
  }
}

# Finding 2: var.bucket_name is referenced only inside a provisioner nested block.
# Without nested-block traversal, zero var_ref edges are emitted.
resource "null_resource" "provisioner_consumer" {
  provisioner "local-exec" {
    command = var.bucket_name
  }
}
