// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package anthropic

import (
	"context"
	"fmt"
	"os"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/vertex"
	"golang.org/x/oauth2/google"

	"github.com/go-steer/core-agent/pkg/config"
	"github.com/go-steer/core-agent/pkg/models"
)

// Env vars consulted by the Vertex constructor when project / region
// are not supplied explicitly via config. Names match Anthropic SDK
// conventions; the GCP-standard fallbacks let the same env that drives
// Vertex Gemini also drive Vertex Anthropic.
const (
	EnvVertexProject = "ANTHROPIC_VERTEX_PROJECT_ID"
	EnvVertexRegion  = "CLOUD_ML_REGION"
)

// Default Vertex region for Claude. Most current Anthropic Vertex
// deployments live in us-east5; override per call site as needed.
const DefaultVertexRegion = "us-east5"

func init() {
	models.Register(config.ProviderAnthropicVertex, newVertexProvider)
}

// NewVertex constructs a Provider that talks to Claude via Google
// Vertex AI. project and region are required. Authentication uses
// Application Default Credentials (run `gcloud auth application-default
// login`, or set GOOGLE_APPLICATION_CREDENTIALS, or rely on workload
// identity in production).
//
// We deliberately load credentials via google.FindDefaultCredentials
// ourselves and pass them to vertex.WithCredentials — vertex.WithGoogleAuth
// panics on missing creds, which we don't want at startup.
func NewVertex(ctx context.Context, project, region string, opts ...Option) (*Provider, error) {
	if project == "" {
		return nil, fmt.Errorf("anthropic-vertex: project is required (set model.anthropic.vertex.project in .agents/config.json or %s env)", EnvVertexProject)
	}
	if region == "" {
		return nil, fmt.Errorf("anthropic-vertex: region is required (set model.anthropic.vertex.location in .agents/config.json or %s env)", EnvVertexRegion)
	}
	creds, err := google.FindDefaultCredentials(ctx, "https://www.googleapis.com/auth/cloud-platform")
	if err != nil {
		return nil, fmt.Errorf("anthropic-vertex: load default credentials: %w (run `gcloud auth application-default login`)", err)
	}
	p := &Provider{
		name:     config.ProviderAnthropicVertex,
		client:   anthropic.NewClient(vertex.WithCredentials(ctx, region, project, creds)),
		builtins: DefaultBuiltinTools(),
	}
	for _, opt := range opts {
		opt(p)
	}
	return p, nil
}

// newVertexProvider is the registry constructor for "anthropic-vertex".
// Project and region are resolved in this order:
//  1. cfg.Model.Anthropic.Vertex
//  2. ANTHROPIC_VERTEX_PROJECT_ID / CLOUD_ML_REGION
//  3. GOOGLE_CLOUD_PROJECT / GOOGLE_CLOUD_LOCATION (shared with Gemini Vertex)
//  4. DefaultVertexRegion as a final fallback for region only
func newVertexProvider(cfg *config.Config) (models.Provider, error) {
	project, region := "", ""
	if cfg.Model.Anthropic != nil && cfg.Model.Anthropic.Vertex != nil {
		project = cfg.Model.Anthropic.Vertex.Project
		region = cfg.Model.Anthropic.Vertex.Location
	}
	if project == "" {
		project = firstNonEmpty(os.Getenv(EnvVertexProject), os.Getenv("GOOGLE_CLOUD_PROJECT"))
	}
	if region == "" {
		region = firstNonEmpty(os.Getenv(EnvVertexRegion), os.Getenv("GOOGLE_CLOUD_LOCATION"), DefaultVertexRegion)
	}
	return NewVertex(context.Background(), project, region)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}
