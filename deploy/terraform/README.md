# Terraform modules

Infrastructure-as-Code for forge's cloud dependencies: managed Postgres and object storage.

## Modules

| Directory | Cloud | Resources |
|-----------|-------|-----------|
| `aws/` | AWS | RDS PostgreSQL 16, S3 bucket, IAM user + policy |
| `gcp/` | GCP | Cloud SQL PostgreSQL 16, GCS bucket, service account + key |

All modules output the same logical values under consistent names so Helm values look the same regardless of cloud:

| Output | Helm value / env var |
|--------|----------------------|
| `postgres_dsn` | `POSTGRES_DSN` (Kubernetes Secret) |
| `s3_bucket_name` / `gcs_bucket_name` | `S3_BUCKET` |
| `s3_access_key_id` | `S3_ACCESS_KEY` |
| `s3_secret_access_key` / `service_account_key_json` | `S3_SECRET_KEY` |

## Usage — AWS

```bash
cd deploy/terraform/aws

# Provide required variables (never commit db_password)
cat > terraform.tfvars <<EOF
vpc_id     = "vpc-0abc123"
subnet_ids = ["subnet-0abc123", "subnet-0def456"]
db_password = "$(openssl rand -base64 32)"
EOF

terraform init
terraform plan
terraform apply

# Store sensitive outputs in Kubernetes Secrets
terraform output -raw postgres_dsn     # → POSTGRES_DSN
terraform output -raw s3_secret_access_key  # → S3_SECRET_KEY
```

Then install forge with external storage:

```bash
helm upgrade --install forge deploy/helm/forge \
  --set storage.type=external \
  --set extraEnv.S3_BUCKET="$(terraform output -raw s3_bucket_name)" \
  --set extraEnv.S3_REGION="$(terraform output -raw s3_region)" \
  --set extraEnv.S3_ACCESS_KEY="$(terraform output -raw s3_access_key_id)" \
  --set-string extraEnv.S3_ENDPOINT="https://s3.amazonaws.com" \
  --set-json 'extraEnvFrom=[{"secretRef":{"name":"forge-secrets"}}]'
```

## Usage — GCP

```bash
cd deploy/terraform/gcp

cat > terraform.tfvars <<EOF
project     = "my-gcp-project"
db_password = "$(openssl rand -base64 32)"
EOF

terraform init
terraform plan
terraform apply

terraform output -raw postgres_dsn             # → POSTGRES_DSN
terraform output -raw service_account_key_json # → GOOGLE_APPLICATION_CREDENTIALS
```

## IRSA / Workload Identity (no static credentials)

**AWS (IRSA):** attach `iam_policy_arn` output to your EKS pod IAM role instead of creating the IAM user.

**GCP (Workload Identity):** annotate the Kubernetes service account with the GSA email from `service_account_email` output — skip creating the service account key.

## State backend

Always use remote state for shared infrastructure. Add a `backend` block to `versions.tf`:

```hcl
# AWS
terraform {
  backend "s3" {
    bucket = "my-tfstate"
    key    = "forge/terraform.tfstate"
    region = "us-east-1"
  }
}

# GCP
terraform {
  backend "gcs" {
    bucket = "my-tfstate"
    prefix = "forge"
  }
}
```
