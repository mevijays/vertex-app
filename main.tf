# =============================================================================
# Terraform / provider configuration
# =============================================================================

terraform {
  required_version = ">= 1.5.0"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = "~> 7.0"
    }
  }
}

provider "google" {
  project = var.project_id
}

# =============================================================================
# Variables
# =============================================================================

variable "project_id" {
  description = "ID of the Google Cloud project in which to create the resources."
  type        = string

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.project_id))
    error_message = "project_id must be a valid Google Cloud project ID."
  }
}

variable "service_account_id" {
  description = "Account ID for the service account."
  type        = string
  default     = "vertex-gemini-client"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{4,28}[a-z0-9]$", var.service_account_id))
    error_message = "service_account_id must be 6-30 lowercase letters, digits, or hyphens, start with a letter, and end with a letter or digit."
  }
}

variable "service_account_display_name" {
  description = "Display name for the service account."
  type        = string
  default     = "Vertex AI Gemini client"
}

variable "custom_role_id" {
  description = "ID for the project-level custom IAM role. Hyphens are not allowed."
  type        = string
  default     = "vertexGeminiClient"

  validation {
    condition     = can(regex("^[A-Za-z0-9_.]{3,64}$", var.custom_role_id))
    error_message = "custom_role_id must contain 3-64 letters, digits, underscores, or periods; hyphens are not allowed."
  }
}

variable "custom_role_title" {
  description = "Human-readable title for the custom IAM role."
  type        = string
  default     = "Vertex AI Gemini Client"
}

variable "enable_vision_permissions" {
  description = "Include permissions to get and list Vertex AI models for image analysis."
  type        = bool
  default     = false
}

variable "enable_gcs_read_permissions" {
  description = "Include permissions to read Cloud Storage bucket metadata and objects."
  type        = bool
  default     = false
}

variable "region" {
  description = "Google Cloud region for Artifact Registry."
  type        = string
  default     = "asia-south1"
}

variable "vertex_ai_location" {
  description = "Vertex AI inference endpoint location. Gemini 3.5 Flash supports the global endpoint."
  type        = string
  default     = "global"
}

variable "gemini_model_id" {
  description = "Vertex AI publisher model ID used by the application."
  type        = string
  default     = "gemini-3.5-flash"
}

variable "artifact_registry_repository_id" {
  description = "Artifact Registry Docker repository ID."
  type        = string
  default     = "vertex-chat"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,61}[a-z0-9]$", var.artifact_registry_repository_id))
    error_message = "artifact_registry_repository_id must contain 3-63 lowercase letters, digits, or hyphens."
  }
}

variable "gke_cluster_name" {
  description = "Name of the GKE cluster and the prefix for its fleet membership ID."
  type        = string
  default     = "vertex-chat-gke"

  validation {
    condition     = can(regex("^[a-z][a-z0-9-]{1,38}[a-z0-9]$", var.gke_cluster_name))
    error_message = "gke_cluster_name must be 3-40 lowercase letters, digits, or hyphens, start with a letter, and end with a letter or digit."
  }
}

variable "gke_zone" {
  description = "Zone for the standard (zonal) GKE cluster."
  type        = string
  default     = "us-central1-a"
}

variable "gke_node_count" {
  description = "Initial node count for the cluster's default node pool. Only used at pool creation — see gke_node_min_count/gke_node_max_count for ongoing autoscaling bounds."
  type        = number
  default     = 3
}

variable "gke_node_min_count" {
  description = "Minimum nodes in default-pool once autoscaling is enabled. Matches the pool's original fixed size, so enabling autoscaling never scales below what was already running."
  type        = number
  default     = 3
}

variable "gke_node_max_count" {
  description = "Maximum nodes in default-pool. Sized for headroom beyond the app alone (e.g. an Istio ingress gateway) without over-provisioning."
  type        = number
  default     = 5
}

variable "gke_deletion_protection" {
  description = "Prevent Terraform from deleting the GKE cluster. Must be set to false before a terraform destroy can remove it."
  type        = bool
  default     = true
}

variable "gke_workload_identity_namespace" {
  description = "Kubernetes namespace of the ServiceAccount allowed to impersonate the runtime GSA via Workload Identity Federation."
  type        = string
  default     = "default"
}

variable "gke_workload_identity_ksa" {
  description = "Kubernetes ServiceAccount name allowed to impersonate the runtime GSA via Workload Identity Federation. Must match the ServiceAccount name in k8s/deployment.yaml."
  type        = string
  default     = "vertex-chat-ksa"
}

variable "github_repository" {
  description = "GitHub repository allowed to assume the CI/CD deploy identity via Workload Identity Federation, in \"owner/repo\" format (e.g. mevijays/vertex-chat). Only this exact repository can impersonate the deploy service account."
  type        = string

  validation {
    condition     = can(regex("^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$", var.github_repository))
    error_message = "github_repository must be in \"owner/repo\" format."
  }
}

variable "deploy_service_account_id" {
  description = "Account ID for the GitHub Actions GKE-deploy service account."
  type        = string
  default     = "gke-deployer"
}

# =============================================================================
# Required APIs and Artifact Registry
# =============================================================================

locals {
  required_services = toset([
    "aiplatform.googleapis.com",
    "artifactregistry.googleapis.com",
    "cloudbuild.googleapis.com",
    "compute.googleapis.com",
    "container.googleapis.com",
    "gkehub.googleapis.com",
    "iam.googleapis.com",
    "iamcredentials.googleapis.com",
    "mesh.googleapis.com",
    "sts.googleapis.com",
  ])
}

resource "google_project_service" "required" {
  for_each = local.required_services

  project            = var.project_id
  service            = each.value
  disable_on_destroy = false
}

resource "google_artifact_registry_repository" "app" {
  project       = var.project_id
  location      = var.region
  repository_id = var.artifact_registry_repository_id
  description   = "Container images for Vertex Chat"
  format        = "DOCKER"

  depends_on = [google_project_service.required]
}

# =============================================================================
# Runtime service account, least-privilege custom role, and IAM binding
# =============================================================================

locals {
  core_permissions = [
    "aiplatform.endpoints.predict",
  ]

  vision_permissions = var.enable_vision_permissions ? [
    "aiplatform.models.get",
    "aiplatform.models.list",
  ] : []

  gcs_read_permissions = var.enable_gcs_read_permissions ? [
    "storage.buckets.get",
    "storage.objects.get",
  ] : []

  custom_role_permissions = concat(
    local.core_permissions,
    local.vision_permissions,
    local.gcs_read_permissions,
  )
}

resource "google_service_account" "vertex_gemini" {
  project      = var.project_id
  account_id   = var.service_account_id
  display_name = var.service_account_display_name
  description  = "Calls the Vertex AI Gemini API with least-privilege access."

  depends_on = [google_project_service.required["iam.googleapis.com"]]
}

resource "google_project_iam_custom_role" "vertex_gemini" {
  project     = var.project_id
  role_id     = var.custom_role_id
  title       = var.custom_role_title
  description = "Least-privilege access for calling Gemini through Vertex AI."
  permissions = local.custom_role_permissions
  stage       = "GA"

  depends_on = [google_project_service.required["iam.googleapis.com"]]
}

# Grant the custom role to the service account at project scope. This resource is
# non-authoritative, so it does not replace other members assigned to this role.
resource "google_project_iam_member" "vertex_gemini" {
  project = var.project_id
  role    = google_project_iam_custom_role.vertex_gemini.name
  member  = "serviceAccount:${google_service_account.vertex_gemini.email}"
}

# Workload Identity Federation: lets the GKE pod's KSA impersonate this GSA
# with no key, matching the ServiceAccount annotated in k8s/deployment.yaml.
# Terraform equivalent of:
#   gcloud iam service-accounts add-iam-policy-binding \
#     ${google_service_account.vertex_gemini.email} \
#     --role roles/iam.workloadIdentityUser \
#     --member "serviceAccount:${var.project_id}.svc.id.goog[${namespace}/${ksa}]"
resource "google_service_account_iam_member" "vertex_gemini_workload_identity" {
  service_account_id = google_service_account.vertex_gemini.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "serviceAccount:${var.project_id}.svc.id.goog[${var.gke_workload_identity_namespace}/${var.gke_workload_identity_ksa}]"

  depends_on = [google_container_cluster.gke]
}

# =============================================================================
# Cloud Build default service account permissions
# =============================================================================

data "google_project" "current" {
  project_id = var.project_id
}

locals {
  # Cloud Build runs as the default Compute Engine service account unless a
  # dedicated build service account is configured. Newer projects do not
  # auto-grant this identity any IAM roles, so it needs explicit access to
  # read its own build-staging bucket and push images to Artifact Registry.
  cloudbuild_default_sa = "serviceAccount:${data.google_project.current.number}-compute@developer.gserviceaccount.com"
}

resource "google_project_iam_member" "cloudbuild_storage_reader" {
  project = var.project_id
  role    = "roles/storage.objectViewer"
  member  = local.cloudbuild_default_sa
}

resource "google_project_iam_member" "cloudbuild_artifact_writer" {
  project = var.project_id
  role    = "roles/artifactregistry.writer"
  member  = local.cloudbuild_default_sa
}

# =============================================================================
# Standard (zonal) GKE cluster, Fleet registration, and Managed Cloud Service
# Mesh — Terraform equivalent of:
#
#   gcloud container clusters create ${var.gke_cluster_name} \
#     --zone us-central1-a \
#     --workload-pool=${var.project_id}.svc.id.goog \
#     --enable-managed-prometheus \
#     --monitoring=SYSTEM,POD \
#     --no-enable-network-policy
#
#   gcloud container fleet memberships register ${var.gke_cluster_name}-membership \
#     --gke-cluster=us-central1-a/${var.gke_cluster_name} \
#     --enable-workload-identity
#
#   gcloud container fleet mesh enable
#
#   gcloud container fleet mesh update \
#     --management automatic \
#     --memberships ${var.gke_cluster_name}-membership
# =============================================================================

resource "google_container_cluster" "gke" {
  project  = var.project_id
  name     = var.gke_cluster_name
  location = var.gke_zone

  # Standard cluster (not Autopilot). No node_pool block is defined, so this
  # manages the implicit default node pool the same way `gcloud container
  # clusters create` does — sized only, machine type left at the API default.
  initial_node_count  = var.gke_node_count
  deletion_protection = var.gke_deletion_protection

  # --workload-pool=PROJECT_ID.svc.id.goog
  workload_identity_config {
    workload_pool = "${var.project_id}.svc.id.goog"
  }

  # --enable-managed-prometheus --monitoring=SYSTEM,POD
  monitoring_config {
    enable_components = ["SYSTEM_COMPONENTS", "POD"]
    managed_prometheus {
      enabled = true
    }
  }

  # --no-enable-network-policy
  network_policy {
    enabled = false
  }

  depends_on = [google_project_service.required]
}

# Adopts the cluster's default-pool as an explicitly managed resource so it
# can carry autoscaling — google_container_cluster has no argument for that on
# an implicit default pool. This resource is meant to be imported onto an
# already-existing pool (see README), never created fresh: setting
# remove_default_node_pool on the cluster to force a "clean" create/import
# would delete and recreate all nodes, taking the running deployment down for
# no operational benefit over an import.
resource "google_container_node_pool" "default" {
  project  = var.project_id
  cluster  = google_container_cluster.gke.name
  location = var.gke_zone
  name     = "default-pool"

  autoscaling {
    min_node_count = var.gke_node_min_count
    max_node_count = var.gke_node_max_count
  }

  lifecycle {
    ignore_changes = [node_config, initial_node_count]
  }
}

# gcloud container fleet memberships register ${cluster}-membership \
#   --gke-cluster=${zone}/${cluster} --enable-workload-identity
resource "google_gke_hub_membership" "gke" {
  project       = var.project_id
  membership_id = "${var.gke_cluster_name}-membership"
  location      = "global"

  endpoint {
    gke_cluster {
      resource_link = "//container.googleapis.com/${google_container_cluster.gke.id}"
    }
  }

  # --enable-workload-identity
  authority {
    issuer = "https://container.googleapis.com/v1/${google_container_cluster.gke.id}"
  }

  depends_on = [google_project_service.required]
}

# gcloud container fleet mesh enable
resource "google_gke_hub_feature" "mesh" {
  project  = var.project_id
  name     = "servicemesh"
  location = "global"

  depends_on = [google_project_service.required]
}

# gcloud container fleet mesh update --management automatic \
#   --memberships ${cluster}-membership
resource "google_gke_hub_feature_membership" "mesh" {
  project    = var.project_id
  location   = "global"
  feature    = google_gke_hub_feature.mesh.name
  membership = google_gke_hub_membership.gke.membership_id

  mesh {
    management = "MANAGEMENT_AUTOMATIC"
  }
}

# =============================================================================
# GitHub Actions Workload Identity Federation + deploy service account
#
# Lets the deploy.yml workflow authenticate to GCP with no stored key: GitHub
# mints a short-lived OIDC token for the run, which this pool/provider trusts
# in exchange for impersonating the deploy GSA below — but only for workflow
# runs from var.github_repository specifically (see attribute_condition).
# =============================================================================

resource "google_iam_workload_identity_pool" "github_actions" {
  project                   = var.project_id
  workload_identity_pool_id = "github-actions"
  display_name              = "GitHub Actions"
  description               = "Workload Identity Federation pool for GitHub Actions OIDC tokens."

  depends_on = [google_project_service.required]
}

resource "google_iam_workload_identity_pool_provider" "github_actions" {
  project                            = var.project_id
  workload_identity_pool_id          = google_iam_workload_identity_pool.github_actions.workload_identity_pool_id
  workload_identity_pool_provider_id = "github"
  display_name                       = "GitHub"

  attribute_mapping = {
    "google.subject"       = "assertion.sub"
    "attribute.repository" = "assertion.repository"
    "attribute.actor"      = "assertion.actor"
    "attribute.ref"        = "assertion.ref"
  }

  # Restrict to this exact repository — without this, ANY GitHub Actions
  # workflow from ANY repository could request tokens against this provider.
  attribute_condition = "assertion.repository == \"${var.github_repository}\""

  oidc {
    issuer_uri = "https://token.actions.githubusercontent.com"
  }
}

resource "google_service_account" "gke_deployer" {
  project      = var.project_id
  account_id   = var.deploy_service_account_id
  display_name = "GitHub Actions GKE deployer"
  description  = "Deploys to GKE from GitHub Actions via Workload Identity Federation. No key is ever created for this account."

  depends_on = [google_project_service.required["iam.googleapis.com"]]
}

# Lets workflow runs from var.github_repository impersonate this GSA — no key involved.
resource "google_service_account_iam_member" "gke_deployer_wif_binding" {
  service_account_id = google_service_account.gke_deployer.name
  role               = "roles/iam.workloadIdentityUser"
  member             = "principalSet://iam.googleapis.com/${google_iam_workload_identity_pool.github_actions.name}/attribute.repository/${var.github_repository}"
}

# Least privilege for `kubectl` access (via get-gke-credentials + apply) —
# roles/container.developer, not container.admin: it can manage workloads on
# existing clusters but cannot create/delete/reconfigure the cluster itself.
resource "google_project_iam_member" "gke_deployer_container_developer" {
  project = var.project_id
  role    = "roles/container.developer"
  member  = "serviceAccount:${google_service_account.gke_deployer.email}"
}

# =============================================================================
# Outputs
# =============================================================================

output "service_account_email" {
  description = "Email address of the created service account."
  value       = google_service_account.vertex_gemini.email
}

output "service_account_name" {
  description = "Fully qualified name of the created service account."
  value       = google_service_account.vertex_gemini.name
}

output "custom_role_name" {
  description = "Fully qualified name of the created custom IAM role."
  value       = google_project_iam_custom_role.vertex_gemini.name
}

output "custom_role_permissions" {
  description = "Permissions included in the custom IAM role."
  value       = google_project_iam_custom_role.vertex_gemini.permissions
}

output "artifact_registry_repository" {
  description = "Artifact Registry repository path used for application images."
  value       = "${var.region}-docker.pkg.dev/${var.project_id}/${google_artifact_registry_repository.app.repository_id}"
}

output "gke_cluster_name" {
  description = "Name of the GKE cluster."
  value       = google_container_cluster.gke.name
}

output "gke_cluster_endpoint" {
  description = "Endpoint of the GKE cluster control plane."
  value       = google_container_cluster.gke.endpoint
  sensitive   = true
}

output "gke_fleet_membership_id" {
  description = "Fleet membership ID the cluster is registered under. Check mesh provisioning status with: gcloud container fleet mesh describe --project <project_id>."
  value       = google_gke_hub_membership.gke.membership_id
}

output "github_actions_workload_identity_provider" {
  description = "Set this as the GitHub repo variable GCP_WORKLOAD_IDENTITY_PROVIDER (used by .github/workflows/deploy.yml)."
  value       = google_iam_workload_identity_pool_provider.github_actions.name
}

output "github_actions_deploy_service_account" {
  description = "Set this as the GitHub repo variable GCP_DEPLOY_SERVICE_ACCOUNT (used by .github/workflows/deploy.yml)."
  value       = google_service_account.gke_deployer.email
}
