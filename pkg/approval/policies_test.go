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

package approval

import (
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/config"
)

func TestWithToolPolicies_WiresMutatingHintPreconditionAndCapture(t *testing.T) {
	t.Parallel()
	mutTrue := true
	mutFalse := false
	policies := []config.ToolPolicyConfig{
		{
			Name:     "get_k8s_resource",
			Mutating: &mutFalse,
		},
		{
			Name:     "file",
			Mutating: &mutTrue, // must NOT suffix-match built-in read_file
		},
		{
			Name:     "gke_get_dup",
			Mutating: &mutFalse,
		},
		{
			Name:     "gke_patch_dup",
			Mutating: &mutTrue,
			Precondition: &config.ToolPreconditionConfig{
				Read: "gke_get_dup",
			},
		},
		{
			Name:           "patch_k8s_resource",
			Mutating:       &mutTrue,
			ParkHintFields: []string{"namespace", "name"},
			Precondition: &config.ToolPreconditionConfig{
				Read:     "get_k8s_resource",
				ArgsFrom: map[string]string{"namespace": "namespace", "name": "name"},
				Fields:   []string{"metadata.resourceVersion"},
			},
			Capture: &config.ToolCaptureConfig{
				Read:     "get_k8s_resource",
				ArgsFrom: map[string]string{"namespace": "namespace", "name": "name"},
				Fields:   []string{"spec.replicas"},
				Revert: &config.ToolRevertConfig{
					Call:            "patch_k8s_resource",
					ArgsFromChange:  map[string]string{"namespace": "namespace", "name": "name"},
					ArgsFromCapture: map[string]string{"replicas": "spec.replicas"},
				},
			},
		},
	}

	var cfg Config
	WithToolPolicies(policies)(&cfg)

	if cfg.Mutating == nil {
		t.Fatal("cfg.Mutating is nil")
	}
	if cfg.Mutating("gke_get_k8s_resource") {
		t.Error("Mutating(gke_get_k8s_resource) = true, want false")
	}
	if !cfg.Mutating("gke_patch_k8s_resource") {
		t.Error("Mutating(gke_patch_k8s_resource) = false, want true")
	}
	if cfg.Mutating("finish_task") {
		t.Error("Mutating(finish_task) = true, want false for control call")
	}
	if cfg.Mutating("read_file") {
		t.Error("Mutating(read_file) = true, want false (built-in tool must not suffix-match 'file')")
	}

	dupPre, err := cfg.Grants.Precondition("gke_gke_patch_dup")
	if err != nil || dupPre == nil || dupPre.Read != "gke_gke_get_dup" {
		t.Errorf("Precondition(gke_gke_patch_dup) = %+v, %v; want Read=gke_gke_get_dup", dupPre, err)
	}

	if cfg.ParkHintFields == nil {
		t.Fatal("cfg.ParkHintFields is nil")
	}
	wg := &writeGate{cfg: cfg}
	gotHint := wg.hintKey("gke_patch_k8s_resource", "full_key", map[string]any{
		"namespace": "prod",
		"name":      "payments",
		"patch":     "{}",
	})
	wantHint := "gke_patch_k8s_resource(name=payments, namespace=prod)"
	if gotHint != wantHint {
		t.Errorf("hintKey = %q, want %q", gotHint, wantHint)
	}

	if cfg.Grants == nil || cfg.Grants.Precondition == nil {
		t.Fatal("cfg.Grants.Precondition is nil")
	}
	pre, err := cfg.Grants.Precondition("gke_patch_k8s_resource")
	if err != nil || pre == nil {
		t.Fatalf("Precondition(gke_patch_k8s_resource) = %+v, %v", pre, err)
	}
	if pre.Read != "gke_get_k8s_resource" {
		t.Errorf("Precondition.Read = %q, want %q", pre.Read, "gke_get_k8s_resource")
	}

	if cfg.Captures == nil || cfg.Captures.For == nil {
		t.Fatal("cfg.Captures.For is nil")
	}
	capRule, err := cfg.Captures.For("gke_patch_k8s_resource")
	if err != nil || capRule == nil || capRule.Revert == nil {
		t.Fatalf("Captures.For(gke_patch_k8s_resource) = %+v, %v", capRule, err)
	}
	if capRule.Read != "gke_get_k8s_resource" {
		t.Errorf("Capture.Read = %q, want %q", capRule.Read, "gke_get_k8s_resource")
	}
	if capRule.Revert.Call != "gke_patch_k8s_resource" {
		t.Errorf("Capture.Revert.Call = %q, want %q", capRule.Revert.Call, "gke_patch_k8s_resource")
	}
}
