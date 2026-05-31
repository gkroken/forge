output "s3_bucket_name" {
  description = "S3 bucket name — set as S3_BUCKET in forge's environment"
  value       = aws_s3_bucket.blobs.bucket
}

output "s3_region" {
  description = "AWS region — set as S3_REGION (or AWS_DEFAULT_REGION)"
  value       = var.region
}

output "s3_access_key_id" {
  description = "IAM access key ID — set as S3_ACCESS_KEY (skip for IRSA)"
  value       = aws_iam_access_key.forge.id
}

output "s3_secret_access_key" {
  description = "IAM secret access key — set as S3_SECRET_KEY in a Kubernetes Secret (skip for IRSA)"
  value       = aws_iam_access_key.forge.secret
  sensitive   = true
}

output "postgres_host" {
  description = "RDS endpoint hostname"
  value       = aws_db_instance.forge.address
}

output "postgres_dsn" {
  description = "Full DSN — set as POSTGRES_DSN in a Kubernetes Secret"
  value       = "postgres://forge:${var.db_password}@${aws_db_instance.forge.address}:5432/forge?sslmode=require"
  sensitive   = true
}

output "iam_policy_arn" {
  description = "Attach this policy to your EKS pod IAM role when using IRSA instead of the IAM user"
  value       = aws_iam_policy.forge.arn
}
