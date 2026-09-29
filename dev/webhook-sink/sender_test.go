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
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/tools/alert"
)

// The T2 rig's contract with the daemon, pinned from the sender's side:
// a target with template "generic", url_env and auth.bearer_env, exactly
// as the rig writes it, is valid config, reaches this sink over loopback
// http, and lands with details.request_id where the rig's grade reads
// it. If the sender grows a loopback block, an https requirement or a
// new body shape, this fails here rather than as a silent T2 FAIL after
// an hour of operator time.
func TestAlertSenderDeliversToTheSink(t *testing.T) {
	dir := t.TempDir()
	o := options{
		addr: "127.0.0.1:0", urlFile: filepath.Join(dir, "url"), out: filepath.Join(dir, "out.jsonl"),
		bearerEnv: "SELFDEV_SINK_TOKEN",
	}
	t.Setenv("SELFDEV_SINK_TOKEN", "tok-123")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, o, os.Getenv, ready) }()
	var url string
	select {
	case url = <-ready:
	case err := <-done:
		t.Fatalf("sink exited early: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("sink never became ready")
	}
	t.Setenv("SELFDEV_SINK_URL", url)

	raw := `{
	  "version": 1,
	  "alerts": {"targets": [{
	    "name": "selfdev-sink",
	    "url_env": "SELFDEV_SINK_URL",
	    "template": "generic",
	    "auth": {"bearer_env": "SELFDEV_SINK_TOKEN"}
	  }]},
	  "permissions": {"mode": "ask", "approval_notify": "selfdev-sink"}
	}`
	agentsDir := filepath.Join(dir, ".agents")
	if err := os.Mkdir(agentsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "config.json"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(agentsDir)
	if err != nil {
		t.Fatalf("the rig's config shape does not load: %v", err)
	}

	snd, err := alert.NewSender(cfg, "selfdev-sink")
	if err != nil {
		t.Fatalf("NewSender: %v", err)
	}
	if err := snd.Send(context.Background(), "warning", "core-agent: write_file is waiting for approval and nobody is attached",
		map[string]any{"request_id": "perm-42", "tool": "write_file"}, "sess-1"); err != nil {
		t.Fatalf("Send: %v", err)
	}

	data, err := os.ReadFile(o.out)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("got %d deliveries, want 1: %q", len(lines), data)
	}
	var rec struct {
		Body struct {
			Level   string         `json:"level"`
			Summary string         `json:"summary"`
			Details map[string]any `json:"details"`
		} `json:"body"`
	}
	if err := json.Unmarshal([]byte(lines[0]), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Body.Details["request_id"] != "perm-42" || rec.Body.Level != "warning" ||
		!strings.Contains(rec.Body.Summary, "nobody is attached") {
		t.Fatalf("delivery = %s", lines[0])
	}
}
