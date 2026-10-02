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

package mcp

import (
	"context"
	"slices"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/childenv"
)

// #1157. A stdio MCP server is third-party code. It used to get the
// daemon's environment only implicitly — exec.Cmd.Env left nil, which
// inherits everything — whenever its mcp.json entry had no `env` block,
// which is most of them. Both shapes must now start from the scrubbed
// environment, and an explicit `env` entry must still win, since that is
// how an operator deliberately hands a server a value.
//
// Not parallel: t.Setenv.
func TestStdioServersNeverInheritAWithheldCredential(t *testing.T) {
	const name = "CORE_AGENT_1157_MCP_TOKEN"
	t.Setenv(name, "s3cret")
	childenv.Withhold(name)

	for _, tc := range []struct {
		label string
		env   map[string]string
	}{
		{"no env block", nil},
		{"unrelated env block", map[string]string{"OTHER": "1"}},
	} {
		t.Run(tc.label, func(t *testing.T) {
			_, cmd, err := transportFor(context.Background(), "srv", ServerSpec{Transport: "stdio", Command: "true", Env: tc.env})
			if err != nil {
				t.Fatal(err)
			}
			if cmd.Env == nil {
				t.Fatal("cmd.Env is nil, so the server inherits the daemon's whole environment, credentials included")
			}
			for _, kv := range cmd.Env {
				if kv == name+"=s3cret" {
					t.Fatalf("the stdio server's environment carries the withheld %s", name)
				}
			}
		})
	}

	t.Run("explicit env still wins", func(t *testing.T) {
		_, cmd, err := transportFor(context.Background(), "srv", ServerSpec{Transport: "stdio", Command: "true", Env: map[string]string{name: "handed-over"}})
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Contains(cmd.Env, name+"=handed-over") {
			t.Fatalf("an operator's explicit mcp.json env entry for %s was dropped: %v", name, cmd.Env)
		}
	})
}
