locals {
  tags = {
    Project   = var.name
    ManagedBy = "terraform"
  }
}

data "aws_caller_identity" "current" {}

# ── S3 blob store ─────────────────────────────────────────────────────────────

resource "aws_s3_bucket" "blobs" {
  bucket = "${var.name}-blobs-${data.aws_caller_identity.current.account_id}"
  tags   = local.tags
}

resource "aws_s3_bucket_versioning" "blobs" {
  bucket = aws_s3_bucket.blobs.id
  versioning_configuration {
    status = "Enabled"
  }
}

resource "aws_s3_bucket_lifecycle_configuration" "blobs" {
  bucket = aws_s3_bucket.blobs.id
  rule {
    id     = "expire-old-versions"
    status = "Enabled"
    noncurrent_version_expiration {
      noncurrent_days = 30
    }
  }
}

resource "aws_s3_bucket_public_access_block" "blobs" {
  bucket                  = aws_s3_bucket.blobs.id
  block_public_acls       = true
  block_public_policy     = true
  ignore_public_acls      = true
  restrict_public_buckets = true
}

# ── IAM ───────────────────────────────────────────────────────────────────────

resource "aws_iam_policy" "forge" {
  name        = "${var.name}-s3"
  description = "Least-privilege S3 access for the forge blob store"
  tags        = local.tags

  policy = jsonencode({
    Version = "2012-10-17"
    Statement = [{
      Effect = "Allow"
      Action = [
        "s3:PutObject",
        "s3:GetObject",
        "s3:DeleteObject",
        "s3:ListBucket",
      ]
      Resource = [
        aws_s3_bucket.blobs.arn,
        "${aws_s3_bucket.blobs.arn}/*",
      ]
    }]
  })
}

# IAM user for non-IRSA deployments (docker compose, bare EC2).
# For EKS, skip this and attach aws_iam_policy.forge to your pod IAM role via IRSA.
resource "aws_iam_user" "forge" {
  name = var.name
  tags = local.tags
}

resource "aws_iam_user_policy_attachment" "forge" {
  user       = aws_iam_user.forge.name
  policy_arn = aws_iam_policy.forge.arn
}

resource "aws_iam_access_key" "forge" {
  user = aws_iam_user.forge.name
}

# ── RDS PostgreSQL ────────────────────────────────────────────────────────────

resource "aws_db_subnet_group" "forge" {
  name       = var.name
  subnet_ids = var.subnet_ids
  tags       = local.tags
}

resource "aws_security_group" "rds" {
  name   = "${var.name}-rds"
  vpc_id = var.vpc_id
  tags   = local.tags

  ingress {
    from_port       = 5432
    to_port         = 5432
    protocol        = "tcp"
    security_groups = var.allowed_security_group_id != "" ? [var.allowed_security_group_id] : []
    cidr_blocks     = var.allowed_security_group_id == "" ? ["10.0.0.0/8"] : []
  }

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }
}

resource "aws_db_instance" "forge" {
  identifier            = var.name
  engine                = "postgres"
  engine_version        = "16"
  instance_class        = var.db_instance_class
  allocated_storage     = 20
  max_allocated_storage = 100
  storage_encrypted     = true

  db_name  = "forge"
  username = "forge"
  password = var.db_password

  db_subnet_group_name   = aws_db_subnet_group.forge.name
  vpc_security_group_ids = [aws_security_group.rds.id]
  publicly_accessible    = false

  backup_retention_period   = 7
  deletion_protection       = true
  skip_final_snapshot       = false
  final_snapshot_identifier = "${var.name}-final"

  tags = local.tags
}
