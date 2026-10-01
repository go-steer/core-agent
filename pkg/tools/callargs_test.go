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
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/internal/testutil"
	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// #1175 phase 2: every built-in that reaches gateRequest hands the gate
// the call's full arguments, so ModeAuto's approver can judge it. Each
// marker is something only the full arguments carry — the content
// write_file would write, the replacement edit_file would make — and
// the probe denies, so nothing runs.
func TestBuiltins_PassFullArgsToTheApprover(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "f.txt")
	cfg := fetchCfg([]string{"https://example.com/*"}, nil)
	ctx := testutil.ApproverToolContext()
	for _, tc := range []struct {
		tool     string
		eligible string
		run      func(g *permissions.Gate) error
		want     []string
	}{
		{"bash", "bash:*", func(g *permissions.Gate) error {
			_, err := bashFunc(g, config.DefaultConfig())(ctx, bashArgs{Command: "echo marker-bash"})
			return err
		}, []string{`"command":"echo marker-bash"`}},
		{"write_file", "write_file:*", func(g *permissions.Gate) error {
			_, err := writeFileFunc(g)(ctx, writeFileArgs{Path: path, Content: "marker-content"})
			return err
		}, []string{`"content":"marker-content"`}},
		{"edit_file", "edit_file:*", func(g *permissions.Gate) error {
			_, err := editFileFunc(g)(ctx, editFileArgs{Path: path, OldString: "a", NewString: "marker-new"})
			return err
		}, []string{`"new_string":"marker-new"`, `"old_string":"a"`}},
		{"delete_file", "delete_file:*", func(g *permissions.Gate) error {
			_, err := deleteFileFunc(g)(ctx, deleteFileArgs{Path: path})
			return err
		}, []string{`"path":`}},
		{"fetch_url", "fetch_url:*", func(g *permissions.Gate) error {
			_, err := fetchURLFunc(g, cfg)(ctx, fetchURLArgs{URL: "https://example.com/x", MaxBytes: 4242})
			return err
		}, []string{`"max_bytes":4242`}},
	} {
		t.Run(tc.tool, func(t *testing.T) {
			g, probe := testutil.AutoGateInScope(t, dir, tc.eligible)
			err := tc.run(g)
			if err == nil || !strings.Contains(err.Error(), "approver probe") {
				t.Fatalf("err = %v, want the probe's deny", err)
			}
			probe.RequireArgs(t, tc.tool, tc.want...)
		})
	}
}

// The MCP / skill wrapper passes the arguments the toolset received,
// not only summarizeRequest's 200-byte cut — the injection-past-byte-200
// case decision 3 names.
func TestGatedTool_PassesFullArgsToTheApprover(t *testing.T) {
	t.Parallel()
	for _, readOnly := range []bool{false, true} {
		g, probe := testutil.AutoGate(t, "mcp:*")
		inner := &readOnlyInnerTool{fakeInnerTool: fakeInnerTool{name: "get_pod"}, readOnly: readOnly}
		gt := &gatedTool{inner: inner, gate: g, namespace: "mcp"}
		args := map[string]any{"pad": strings.Repeat("x", 300), "tail": "marker-past-200"}
		if _, err := gt.Run(testutil.ApproverToolContext(), args); err == nil {
			t.Fatalf("readOnly=%v: call ran, want the probe's deny", readOnly)
		}
		probe.RequireArgs(t, "mcp", `"tail":"marker-past-200"`)
		if r := probe.Requests()[0]; strings.Contains(r.Detail, "marker-past-200") {
			t.Errorf("readOnly=%v: Detail carries the marker, so this test cannot tell Args from Detail: %q", readOnly, r.Detail)
		}
	}
}

// A no-parameter MCP call arrives with a nil Args map; it is still a
// whole call, so it reaches the approver as {} rather than escalating
// as a call whose arguments the gate does not hold.
func TestGatedTool_NoArgumentCallReachesTheApprover(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]any{"nil map": map[string]any(nil), "untyped nil": nil} {
		g, probe := testutil.AutoGate(t, "mcp:*")
		gt := &gatedTool{inner: &fakeInnerTool{name: "list_namespaces"}, gate: g, namespace: "mcp"}
		if _, err := gt.Run(testutil.ApproverToolContext(), args); err == nil {
			t.Fatalf("%s: call ran, want the probe's deny", name)
		}
		probe.RequireArgs(t, "mcp", "{}")
	}
}
