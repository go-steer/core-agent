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

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/config"
)

func TestRunConfigImportWorkload(t *testing.T) {
	t.Parallel()
	srcDir := t.TempDir()
	outDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(srcDir, "specialists"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(srcDir, "schemas"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(srcDir, "schemas", "finding.json"), []byte(`{"type":"object","properties":{"title":{"type":"string"}},"required":["title"]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	specMD := "---\nname: diagnoser\ndescription: Diagnoses cluster incidents.\nmode: Task\noutput_schema: ../schemas/finding.json\n---\nDiagnose it.\n"
	if err := os.WriteFile(filepath.Join(srcDir, "specialists", "diagnoser.specialist.md"), []byte(specMD), 0o644); err != nil {
		t.Fatal(err)
	}
	workloadYAML := "name: gke-triage\ndescription: Triage GKE.\nmode: single_session\nspecialists:\n  - diagnoser\nhitl:\n  require_approval: true\n"
	if err := os.WriteFile(filepath.Join(srcDir, "workload.yaml"), []byte(workloadYAML), 0o644); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	code := runConfigSubcommand([]string{"import-workload", "-o", outDir, srcDir}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runConfigSubcommand (-o before dir) exit=%d, stderr=%s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), `imported workload "gke-triage"`) {
		t.Errorf("stdout = %q, want imported message", stdout.String())
	}
	cfg, err := config.Load(filepath.Join(outDir, ".agents"))
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(cfg.Subagents) != 1 || cfg.Subagents[0].Name != "diagnoser" {
		t.Errorf("Subagents = %+v, want [diagnoser]", cfg.Subagents)
	}

	outDir2 := t.TempDir()
	stdout.Reset()
	stderr.Reset()
	code = runConfigSubcommand([]string{"import-workload", srcDir, "--out", outDir2}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runConfigSubcommand (--out after dir) exit=%d, stderr=%s", code, stderr.String())
	}
	if _, err := config.Load(filepath.Join(outDir2, ".agents")); err != nil {
		t.Fatalf("config.Load(outDir2): %v", err)
	}
}

func TestRunConfigSubcommand_Errors(t *testing.T) {
	t.Parallel()
	var stdout, stderr bytes.Buffer
	if code := runConfigSubcommand(nil, &stdout, &stderr); code != 2 {
		t.Errorf("empty args code = %d, want 2", code)
	}
	if code := runConfigSubcommand([]string{"--help"}, &stdout, &stderr); code != 0 {
		t.Errorf("--help code = %d, want 0", code)
	}
	if code := runConfigSubcommand([]string{"unknown"}, &stdout, &stderr); code != 2 {
		t.Errorf("unknown subcommand code = %d, want 2", code)
	}
	if code := runConfigSubcommand([]string{"import-workload"}, &stdout, &stderr); code != 2 {
		t.Errorf("missing workload-dir code = %d, want 2", code)
	}
}
