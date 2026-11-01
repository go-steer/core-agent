# Copyright 2026 Google LLC
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

# Originally derived from go-steer/mast:examples/deploy/terraform/versions.tf.

terraform {
  required_version = ">= 1.9"

  required_providers {
    google = {
      source  = "hashicorp/google"
      version = ">= 6.0"
    }
    kubernetes = {
      source  = "hashicorp/kubernetes"
      version = ">= 2.30"
    }
  }
}

provider "google" {
  project = var.project_id
}

# The kubernetes provider is configured by the ROOT, not by the module, which
# is Terraform's rule for providers and also the right seam here: how you
# reach the cluster is the operator's business. `gcloud container clusters
# get-credentials <cluster>` then the default below is the short path; an
# exec-plugin block against a specific cluster is the CI path. See README.md.
provider "kubernetes" {}
