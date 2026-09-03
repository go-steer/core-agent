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

package tools

import (
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// descriptions maps tool name -> model-facing description for a build.
// The gate is derived from the same cfg so plan mode reaches both the
// registration condition and gate.PlanRequired().
func descriptions(t *testing.T, b BuiltinTools, mutate func(*config.Config)) map[string]string {
	t.Helper()
	cfg := config.DefaultConfig()
	if mutate != nil {
		mutate(cfg)
	}
	gate := permissions.New(permissions.Options{
		Mode:                permissions.ModeYolo,
		RequirePlanArtifact: cfg.Permissions.PlanGateArmed(),
	})
	reg, err := Build(cfg, gate, t.TempDir(), b)
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	out := make(map[string]string, len(reg.Tools))
	for _, tl := range reg.Tools {
		out[tl.Name()] = tl.Description()
	}
	return out
}

// A description that names a tool this build didn't register asserts a
// capability the model doesn't have. Distroless deployments run with no
// shell, so `bash` is the case that actually bites: six tools used to
// tell the model they were "PREFERRED over `bash cat`" / "`bash rm`" /
// "`bash sleep`" in a container where no shell existed.
func TestDescriptions_NoBashReferenceWhenBashDisabled(t *testing.T) {
	t.Parallel()
	b := Default()
	b.Bash = false
	for name, desc := range descriptions(t, b, withURLAllowlist) {
		if strings.Contains(strings.ToLower(desc), "bash") {
			t.Errorf("tool %q description references bash on a build with no bash tool:\n  %s", name, desc)
		}
	}
}

// The mirror of the above: with bash registered the comparisons must
// survive. A fix that strips them unconditionally would pass the test
// above and quietly delete guidance that earns its place.
func TestDescriptions_BashReferencesKeptWhenBashEnabled(t *testing.T) {
	t.Parallel()
	descs := descriptions(t, Default(), withURLAllowlist)
	want := map[string]string{
		"read_file":       "`bash cat`",
		"delete_file":     "`bash rm`",
		"stat":            "`bash stat`",
		"grep":            "`bash grep`",
		"fetch_url":       "`bash curl`",
		"wait_and_verify": "`bash sleep`",
	}
	for name, frag := range want {
		desc, ok := descs[name]
		if !ok {
			t.Errorf("tool %q not registered", name)
			continue
		}
		if !strings.Contains(desc, frag) {
			t.Errorf("tool %q lost its %s comparison on a build WITH bash:\n  %s", name, frag, desc)
		}
	}
}

// bash's own description redirects code investigation to the
// structured tools. Naming one that isn't registered sends the model
// at a tool that isn't there — the failure ActiveNativeSearchTools
// already guards for the search gate.
func TestDescriptions_BashNamesOnlyRegisteredStructuredTools(t *testing.T) {
	t.Parallel()
	b := Default()
	b.Grep = false
	b.ListDir = false
	desc := descriptions(t, b, nil)["bash"]
	for _, absent := range []string{"`grep`", "`list_dir`"} {
		if strings.Contains(desc, absent) {
			t.Errorf("bash description redirects to unregistered %s:\n  %s", absent, desc)
		}
	}
	for _, present := range []string{"`read_file`", "`glob`"} {
		if !strings.Contains(desc, present) {
			t.Errorf("bash description dropped registered %s:\n  %s", present, desc)
		}
	}
}

// Dropping every structured tool must drop the whole redirect clause
// rather than leave a sentence pointing at an empty list.
func TestDescriptions_BashRedirectClauseGoneWhenNoStructuredTools(t *testing.T) {
	t.Parallel()
	desc := descriptions(t, BuiltinTools{Bash: true}, nil)["bash"]
	if strings.Contains(desc, "prefer the structured") {
		t.Errorf("bash keeps a redirect clause with no structured tools to redirect to:\n  %s", desc)
	}
	if !strings.Contains(desc, "shell-native work") {
		t.Errorf("bash lost its base description:\n  %s", desc)
	}
}

// read_many_files' "PREFERRED over multiple parallel `read_file`
// calls" is the same defect with no shell involved.
func TestDescriptions_ReadManyFilesDropsReadFileRefWhenDisabled(t *testing.T) {
	t.Parallel()
	b := Default()
	b.ReadFile = false
	if desc := descriptions(t, b, nil)["read_many_files"]; strings.Contains(desc, "read_file") {
		t.Errorf("read_many_files references unregistered read_file:\n  %s", desc)
	}
	if desc := descriptions(t, Default(), nil)["read_many_files"]; !strings.Contains(desc, "`read_file`") {
		t.Errorf("read_many_files lost its read_file comparison on a build WITH read_file:\n  %s", desc)
	}
}

// record_plan in required mode is the highest-stakes case: it's an
// instruction ("call this BEFORE any ... call"), not a comparison, so
// naming a tool that doesn't exist describes a constraint on nothing.
// spawn_agent must survive — it lives outside tools.Build's catalog,
// where HasTool can't tell "absent" from "unknown".
func TestDescriptions_RecordPlanNamesOnlyRegisteredMutators(t *testing.T) {
	t.Parallel()
	planRequired := func(cfg *config.Config) {
		cfg.Permissions.PlanMode = config.PlanModeRequired
	}
	b := Default()
	b.Bash = false
	b.DeleteFile = false
	desc := descriptions(t, b, planRequired)["record_plan"]
	if desc == "" {
		t.Fatal("record_plan not registered")
	}
	for _, absent := range []string{"bash", "delete_file"} {
		if strings.Contains(desc, absent) {
			t.Errorf("record_plan gating text names unregistered %q:\n  %s", absent, desc)
		}
	}
	for _, present := range []string{"write_file", "edit_file", "spawn_agent"} {
		if !strings.Contains(desc, present) {
			t.Errorf("record_plan gating text dropped %q:\n  %s", present, desc)
		}
	}
}

// A gate that was never told what got registered must keep every
// cross-reference. Hosts that wire tools by hand (and every existing
// caller of NewFetchURLTool / RecordPlan) go through this path, and
// silently stripping their text would be a regression disguised as a
// fix.
func TestDescriptions_UnsetCatalogAssumesRegistered(t *testing.T) {
	t.Parallel()
	gate := permissions.New(permissions.Options{Mode: permissions.ModeYolo})
	if !gate.HasTool("bash") {
		t.Error("HasTool on an unconfigured gate must assume registered")
	}
	var nilGate *permissions.Gate
	if !nilGate.HasTool("bash") {
		t.Error("HasTool on a nil gate must assume registered")
	}
	tl := NewFetchURLTool(gate, config.DefaultConfig())
	if !strings.Contains(tl.Description(), "`bash curl`") {
		t.Errorf("hand-wired fetch_url lost its bash comparison:\n  %s", tl.Description())
	}
}

func withURLAllowlist(cfg *config.Config) {
	cfg.URLScope.Allow = []string{"example.com"}
}
