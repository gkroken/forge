variable "region" {
  type        = string
  description = "AWS region"
  default     = "us-east-1"
}

variable "name" {
  type        = string
  description = "Name prefix applied to all resources"
  default     = "forge"
}

variable "db_instance_class" {
  type        = string
  description = "RDS instance class"
  default     = "db.t4g.micro"
}

variable "db_password" {
  type        = string
  description = "PostgreSQL master password (store in a secret, do not commit)"
  sensitive   = true
}

variable "vpc_id" {
  type        = string
  description = "VPC in which to create the RDS subnet group and security group"
}

variable "subnet_ids" {
  type        = list(string)
  description = "Private subnet IDs for the RDS subnet group (at least two AZs)"
}

variable "allowed_security_group_id" {
  type        = string
  description = "Security group ID of the compute tier (EKS node group, EC2) allowed to connect to RDS port 5432. Leave empty to allow 10.0.0.0/8."
  default     = ""
}
