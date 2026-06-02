locals {
  labels = {
    project    = var.name
    managed_by = "terraform"
  }
}

# ── GCS blob store ────────────────────────────────────────────────────────────

resource "google_storage_bucket" "blobs" {
  name                        = "${var.project}-${var.name}-blobs"
  location                    = var.region
  labels                      = local.labels
  uniform_bucket_level_access = true
  force_destroy               = false

  versioning {
    enabled = true
  }

  lifecycle_rule {
    action { type = "Delete" }
    condition {
      num_newer_versions = 3
      with_state         = "ARCHIVED"
    }
  }
}

# ── Service account ───────────────────────────────────────────────────────────

resource "google_service_account" "forge" {
  account_id   = var.name
  display_name = "forge artifact repository"
  description  = "Used by the forge service to access GCS and Cloud SQL"
}

resource "google_storage_bucket_iam_member" "forge_blobs" {
  bucket = google_storage_bucket.blobs.name
  role   = "roles/storage.objectAdmin"
  member = "serviceAccount:${google_service_account.forge.email}"
}

resource "google_project_iam_member" "forge_cloudsql" {
  project = var.project
  role    = "roles/cloudsql.client"
  member  = "serviceAccount:${google_service_account.forge.email}"
}

# JSON key for non-Workload-Identity deployments (docker compose, bare GCE).
# For GKE with Workload Identity, skip this and bind the KSA to the GSA instead.
resource "google_service_account_key" "forge" {
  service_account_id = google_service_account.forge.name
}

# ── Cloud SQL PostgreSQL ──────────────────────────────────────────────────────

resource "google_sql_database_instance" "forge" {
  name             = var.name
  database_version = "POSTGRES_16"
  region           = var.region
  labels           = local.labels

  settings {
    tier = var.db_tier

    backup_configuration {
      enabled    = true
      start_time = "03:00"
      backup_retention_settings {
        retained_backups = 7
      }
    }

    ip_configuration {
      ipv4_enabled    = false
      private_network = "projects/${var.project}/global/networks/${var.network}"
    }

    insights_config {
      query_insights_enabled = true
    }
  }

  deletion_protection = true
}

resource "google_sql_database" "forge" {
  name     = "forge"
  instance = google_sql_database_instance.forge.name
}

resource "google_sql_user" "forge" {
  name     = "forge"
  instance = google_sql_database_instance.forge.name
  password = var.db_password
}
