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

package ax_test

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/v3/pkg/agentenv"
)

var sha40Re = regexp.MustCompile(`^[0-9a-f]{40}$`)

type axDoc struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name     string `yaml:"name"`
		Atespace string `yaml:"atespace"`
	} `yaml:"metadata"`
	Spec struct {
		Image   string   `yaml:"image"`
		Command []string `yaml:"command"`
		Env     []struct {
			Name  string `yaml:"name"`
			Value string `yaml:"value"`
		} `yaml:"env"`
		Egress []struct {
			Hosts       []string `yaml:"hosts"`
			Credentials []struct {
				Header        string `yaml:"header"`
				Prefix        string `yaml:"prefix"`
				CredentialURI string `yaml:"credentialUri"`
			} `yaml:"credentials"`
		} `yaml:"egress"`
		HTTP struct {
			Port int `yaml:"port"`
		} `yaml:"http"`
		Idle struct {
			SuspendAfter string `yaml:"suspendAfter"`
		} `yaml:"idle"`
		Workspaces []struct {
			Name string `yaml:"name"`
			Path string `yaml:"path"`
		} `yaml:"workspaces"`
	} `yaml:"spec"`
}

func decodeDocs(t *testing.T, path string) []axDoc {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	var docs []axDoc
	for {
		var d axDoc
		err := dec.Decode(&d)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		docs = append(docs, d)
	}
	if len(docs) != 2 || docs[0].Kind != "Task" || docs[1].Kind != "Workspace" {
		t.Fatalf("%s: got %d docs, want [Task, Workspace]", path, len(docs))
	}
	return docs
}

func TestPinsAndDockerfile(t *testing.T) {
	t.Parallel()
	axRefBytes, err := os.ReadFile(".ax-ref")
	if err != nil {
		t.Fatalf("read .ax-ref: %v", err)
	}
	axRef := strings.TrimSpace(string(axRefBytes))
	if !sha40Re.MatchString(axRef) {
		t.Errorf(".ax-ref = %q, want 40-char lowercase hex commit SHA", axRef)
	}

	forkRefBytes, err := os.ReadFile(".ax-fork-ref")
	if err != nil {
		t.Fatalf("read .ax-fork-ref: %v", err)
	}
	forkRef := strings.TrimSpace(string(forkRefBytes))
	if !sha40Re.MatchString(forkRef) {
		t.Errorf(".ax-fork-ref = %q, want 40-char lowercase hex commit SHA", forkRef)
	}

	dfBytes, err := os.ReadFile("Dockerfile")
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}
	df := string(dfBytes)
	for _, want := range []string{
		"ARG AX_REF=" + forkRef,
		`ENTRYPOINT ["/usr/local/bin/ax-task-runner"]`,
		"/out/ax-task-runner",
		"/out/core-agent",
		"/out/ax-agent-entry",
		"/opt/core-agent/recipes/gke-platform-agent",
	} {
		if !strings.Contains(df, want) {
			t.Errorf("Dockerfile missing %q", want)
		}
	}
}

func TestTaskManifests_Contract(t *testing.T) {
	t.Parallel()
	for _, file := range []string{"task.yaml", "task-serve.yaml"} {
		docs := decodeDocs(t, file)
		task := docs[0]
		if len(task.Spec.Command) == 0 {
			t.Fatalf("%s: spec.command is empty", file)
		}
		// AX always runs /usr/local/bin/ax-task-runner as PID 1, which in
		// turn spawns spec.command; naming ax-task-runner in spec.command
		// would recurse into the runner.
		if strings.Contains(task.Spec.Command[0], "ax-task-runner") {
			t.Errorf("%s: spec.command[0] = %q, must not be ax-task-runner (AX starts the runner as PID 1)", file, task.Spec.Command[0])
		}

		// Every durable path flag and workspace mount must live under
		// /workspace so state survives suspend and resume.
		for _, arg := range task.Spec.Command {
			for _, prefix := range []string{"--state-dir=", "--session-db-path=", "--agents-content-dir="} {
				if val, ok := strings.CutPrefix(arg, prefix); ok {
					if !strings.HasPrefix(val, "/workspace/") && val != "/workspace" {
						t.Errorf("%s: %s points outside /workspace: %q", file, prefix, val)
					}
				}
			}
		}
		for _, ws := range task.Spec.Workspaces {
			if !strings.HasPrefix(ws.Path, "/workspace/") && ws.Path != "/workspace" {
				t.Errorf("%s: workspace %q path %q is outside /workspace", file, ws.Name, ws.Path)
			}
		}

		// spec.env is persisted in plain text in the actor template and its
		// snapshots; never place real keys or non-placeholder tokens there.
		for _, e := range task.Spec.Env {
			if strings.Contains(e.Name, "KEY") || strings.Contains(e.Name, "SECRET") {
				t.Errorf("%s: spec.env must not carry key/secret variable %q", file, e.Name)
			}
			if strings.Contains(e.Name, "TOKEN") && !strings.HasPrefix(e.Value, "replace-with-") && e.Value != "placeholder" {
				t.Errorf("%s: token env %q must use a clear placeholder value, got %q", file, e.Name, e.Value)
			}
		}
	}
}

func TestTaskOneshot_UsesEntryWrapper(t *testing.T) {
	t.Parallel()
	task := decodeDocs(t, "task.yaml")[0]
	cmd := task.Spec.Command
	if cmd[0] != "/usr/local/bin/ax-agent-entry" {
		t.Errorf("task.yaml spec.command[0] = %q, want /usr/local/bin/ax-agent-entry", cmd[0])
	}
	joined := strings.Join(cmd, " ")
	for _, want := range []string{
		"--wait-for-egress=",
		"--state-dir=/workspace/.core-agent",
		"-- core-agent",
		"--session-db",
		"--session-db-path=/workspace/.core-agent/sessions.db",
		"-p",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("task.yaml command missing %q: %v", want, cmd)
		}
	}
}

func TestTaskServe_MatchesRecipeAndInjectedAuth(t *testing.T) {
	t.Parallel()
	task := decodeDocs(t, "task-serve.yaml")[0]
	cmd := task.Spec.Command
	if cmd[0] != "core-agent" {
		t.Errorf("task-serve.yaml spec.command[0] = %q, want core-agent", cmd[0])
	}
	joined := strings.Join(cmd, " ")
	for _, want := range []string{
		"-c /opt/core-agent/recipes/gke-platform-agent/.agents/config.json",
		"--no-repl",
		"--attach-listen=127.0.0.1:8484",
		"--attach-token=CORE_AGENT_ATTACH_TOKEN",
		"--session-db",
		"--session-db-path=/workspace/.core-agent/sessions.db",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("task-serve.yaml command missing %q: %v", want, cmd)
		}
	}
	if task.Spec.HTTP.Port != 8484 {
		t.Errorf("task-serve.yaml spec.http.port = %d, want 8484 to match --attach-listen", task.Spec.HTTP.Port)
	}

	envMap := map[string]string{}
	for _, e := range task.Spec.Env {
		envMap[e.Name] = e.Value
	}
	if got := envMap[agentenv.EnvGoogleAuth]; got != agentenv.GoogleAuthInjected {
		t.Errorf("task-serve.yaml %s = %q, want %q", agentenv.EnvGoogleAuth, got, agentenv.GoogleAuthInjected)
	}

	// Verify every required variable declared by gke-platform-agent's
	// .agents/env.yaml is populated in task-serve.yaml's spec.env so
	// startup validation passes out of the box.
	manifest, err := agentenv.LoadManifest(filepath.Join("..", "..", "gke-platform-agent", ".agents"))
	if err != nil {
		t.Fatalf("load gke-platform-agent env.yaml: %v", err)
	}
	for _, entry := range manifest.Env {
		if entry.Required && envMap[entry.Name] == "" {
			t.Errorf("task-serve.yaml spec.env missing required recipe env var %q", entry.Name)
		}
	}
}
