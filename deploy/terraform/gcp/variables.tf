variable "project" {
  type        = string
  description = "GCP project ID"
}

variable "region" {
  type        = string
  description = "GCP region"
  default     = "us-central1"
}

variable "name" {
  type        = string
  description = "Name prefix applied to all resources"
  default     = "forge"
}

variable "db_tier" {
  type        = string
  description = "Cloud SQL machine type"
  default     = "db-f1-micro"
}

variable "db_password" {
  type        = string
  description = "PostgreSQL password for the forge user (store in a secret, do not commit)"
  sensitive   = true
}

variable "network" {
  type        = string
  description = "VPC network name for the private Cloud SQL IP"
  default     = "default"
}
