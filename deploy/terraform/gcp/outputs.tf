output "gcs_bucket_name" {
  description = "GCS bucket name — set as S3_BUCKET in forge's environment (forge uses the S3-compatible GCS API)"
  value       = google_storage_bucket.blobs.name
}

output "service_account_email" {
  description = "Service account email — use for Workload Identity binding in GKE"
  value       = google_service_account.forge.email
}

output "service_account_key_json" {
  description = "Service account key JSON — set as GOOGLE_APPLICATION_CREDENTIALS content in a Kubernetes Secret (skip for Workload Identity)"
  value       = base64decode(google_service_account_key.forge.private_key)
  sensitive   = true
}

output "postgres_instance_connection_name" {
  description = "Cloud SQL instance connection name — used by the Cloud SQL Auth Proxy sidecar"
  value       = google_sql_database_instance.forge.connection_name
}

output "postgres_private_ip" {
  description = "Private IP of the Cloud SQL instance (reachable from the same VPC)"
  value       = google_sql_database_instance.forge.private_ip_address
}

output "postgres_dsn" {
  description = "DSN for direct private-IP access — set as POSTGRES_DSN in a Kubernetes Secret"
  value       = "postgres://forge:${var.db_password}@${google_sql_database_instance.forge.private_ip_address}:5432/forge?sslmode=require"
  sensitive   = true
}
