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
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	adkmodel "google.golang.org/adk/v2/model"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v3/pkg/agentenv"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/models"
)

func TestNewVertex_RequiresProject(t *testing.T) {
	t.Parallel()
	_, err := NewVertex(context.Background(), "", "us-east5")
	if err == nil || !strings.Contains(err.Error(), "project is required") {
		t.Fatalf("expected project-required error, got %v", err)
	}
}

func TestNewVertex_RequiresRegion(t *testing.T) {
	t.Parallel()
	_, err := NewVertex(context.Background(), "my-project", "")
	if err == nil || !strings.Contains(err.Error(), "region is required") {
		t.Fatalf("expected region-required error, got %v", err)
	}
}

func TestResolve_AnthropicVertex_FromConfig(t *testing.T) {
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("CLOUD_ML_REGION", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("GOOGLE_CLOUD_LOCATION", "")

	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderAnthropicVertex
	cfg.Model.Anthropic = &config.AnthropicConfig{
		Vertex: &config.VertexConfig{Project: "my-project", Location: "us-east5"},
	}
	p, err := models.Resolve(cfg)
	if err != nil {
		// Skip when the test machine has no ADC — we're testing the
		// resolver wiring, not the GCP creds load. ADC missing is the
		// expected outcome on most CI machines.
		if strings.Contains(err.Error(), "load default credentials") {
			t.Skipf("no ADC on this machine: %v", err)
		}
		t.Fatalf("Resolve: %v", err)
	}
	if p.Name() != config.ProviderAnthropicVertex {
		t.Errorf("provider name = %q, want %q", p.Name(), config.ProviderAnthropicVertex)
	}
}

func TestResolve_AnthropicVertex_MissingProjectErrors(t *testing.T) {
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "")
	t.Setenv("GOOGLE_CLOUD_PROJECT", "")
	t.Setenv("CLOUD_ML_REGION", "us-east5")

	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderAnthropicVertex
	_, err := models.Resolve(cfg)
	if err == nil || !strings.Contains(err.Error(), "project is required") {
		t.Fatalf("expected project-required error, got %v", err)
	}
}

func TestNewVertexProvider_HonorsEnvFallbacks(t *testing.T) {
	t.Setenv("ANTHROPIC_VERTEX_PROJECT_ID", "from-env")
	t.Setenv("CLOUD_ML_REGION", "europe-west4")

	cfg := config.DefaultConfig()
	cfg.Model.Provider = config.ProviderAnthropicVertex
	// No cfg.Model.Anthropic block — should pick up env vars.
	_, err := newVertexProvider(cfg)
	if err != nil && !strings.Contains(err.Error(), "load default credentials") {
		// Same skip rationale as above.
		t.Fatalf("env fallback path should reach the creds load step, got %v", err)
	}
}

// TestNewVertex_MissingADCGetsTheOperatorWording forces the no-ADC path
// on any machine, ADC or not, so the operator-facing wording (and the
// "load default credentials" text the ADC-less skip guards above key
// on) can't silently disappear. It did once: matching core-models'
// auth.ErrNoADC with errors.Is alone missed, because profile.Resolve
// re-wraps the auth error with %v. Not parallel: t.Setenv.
func TestNewVertex_MissingADCGetsTheOperatorWording(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", t.TempDir()+"/missing.json")
	_, err := NewVertex(context.Background(), "my-project", "us-east5")
	if err == nil {
		t.Fatal("NewVertex succeeded with no credentials")
	}
	for _, want := range []string{"load default credentials", "gcloud auth application-default login"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("missing-ADC error lacks %q: %v", want, err)
		}
	}
}

// TestAnthropicVertex_InjectedGoogleAuthReachesTheRequest verifies that
// CORE_AGENT_GOOGLE_AUTH=injected skips ADC discovery and sends
// "Authorization: Bearer placeholder" on Anthropic Vertex requests.
// Not parallel: t.Setenv.
func TestAnthropicVertex_InjectedGoogleAuthReachesTheRequest(t *testing.T) {
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", t.TempDir()+"/missing.json")
	t.Setenv(agentenv.EnvGoogleAuth, agentenv.GoogleAuthInjected)

	var (
		mu   sync.Mutex
		auth []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		auth = append(auth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4-6","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":10,"output_tokens":2}}`)
	}))
	defer srv.Close()

	p, err := NewVertex(context.Background(), "my-project", "us-east5")
	if err != nil {
		t.Fatalf("NewVertex with injected auth: %v", err)
	}
	p.prof.BaseURL = srv.URL
	m, err := p.Model(context.Background(), "claude-sonnet-4-6")
	if err != nil {
		t.Fatalf("Model: %v", err)
	}
	req := &adkmodel.LLMRequest{Contents: []*genai.Content{genai.NewContentFromText("hi", genai.RoleUser)}}
	for _, err := range m.GenerateContent(context.Background(), req, false) {
		if err != nil {
			t.Fatalf("GenerateContent: %v", err)
		}
	}

	mu.Lock()
	gotAuth := append([]string(nil), auth...)
	mu.Unlock()
	if len(gotAuth) != 1 || gotAuth[0] != "Bearer "+agentenv.InjectedGooglePlaceholder {
		t.Errorf("Authorization = %v, want [Bearer %s]", gotAuth, agentenv.InjectedGooglePlaceholder)
	}
}
