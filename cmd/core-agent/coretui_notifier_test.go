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
	"errors"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/mcp"
)

func TestMCPStartupFailureNotice(t *testing.T) {
	tests := []struct {
		name    string
		servers []*mcp.Server
		want    string
	}{
		{
			name:    "no servers → empty",
			servers: nil,
			want:    "",
		},
		{
			name: "all healthy → empty",
			servers: []*mcp.Server{
				{Name: "github", Status: mcp.StatusOK},
				{Name: "filesystem", Status: mcp.StatusOK},
			},
			want: "",
		},
		{
			name: "single failure with error",
			servers: []*mcp.Server{
				{Name: "github", Status: mcp.StatusOK},
				{Name: "gke", Status: mcp.StatusError, Err: errors.New("connection refused")},
			},
			want: "MCP server failed to start — gke: connection refused",
		},
		{
			name: "multiple failures listed",
			servers: []*mcp.Server{
				{Name: "github", Status: mcp.StatusOK},
				{Name: "gke", Status: mcp.StatusError, Err: errors.New("connection refused")},
				{Name: "linear", Status: mcp.StatusError, Err: errors.New("auth: missing API key")},
			},
			want: "2 MCP servers failed to start:\n  • gke: connection refused\n  • linear: auth: missing API key",
		},
		{
			name: "failure without error message — name only",
			servers: []*mcp.Server{
				{Name: "mystery", Status: mcp.StatusError},
			},
			want: "MCP server failed to start — mystery",
		},
		{
			name: "nil server entries skipped without panic",
			servers: []*mcp.Server{
				nil,
				{Name: "gke", Status: mcp.StatusError, Err: errors.New("boom")},
				nil,
			},
			want: "MCP server failed to start — gke: boom",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := mcpStartupFailureNotice(tc.servers)
			if got != tc.want {
				t.Errorf("mcpStartupFailureNotice() =\n%q\nwant:\n%q", got, tc.want)
			}
			// No-trailing-whitespace invariant for the multi-failure
			// branch — protects the chat-row renderer from a stray
			// blank line under the notice.
			if strings.HasSuffix(got, "\n") {
				t.Errorf("notice should not end with a newline; got %q", got)
			}
		})
	}
}
