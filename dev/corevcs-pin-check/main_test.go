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
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVerify_DetectsMissingAndUnknownGuards(t *testing.T) {
	t.Parallel()
	up := &upstreamInfo{
		Version: "v1.1.0",
		Interfaces: map[string]string{
			"Provider":   "provider.go:20",
			"Credential": "cred.go:10",
		},
		Providers: []string{"git", "github", "gitlab"},
	}

	// Happy path: all interfaces and providers guarded.
	okEntries := []guardEntry{
		{Interface: "Provider", RHS: "(*github.Provider)(nil)", Line: 28},
		{Interface: "Provider", RHS: "(*gitlab.Provider)(nil)", Line: 29},
		{Interface: "Provider", RHS: "(*git.Provider)(nil)", Line: 30},
		{Interface: "Credential", RHS: `corevcs.TokenFile("")`, Line: 31},
	}
	if findings := verify(up, okEntries, nil); len(findings) != 0 {
		t.Fatalf("verify(okEntries) returned unexpected findings: %v", findings)
	}

	// Missing Credential and missing gitlab provider.
	incomplete := []guardEntry{
		{Interface: "Provider", RHS: "(*github.Provider)(nil)", Line: 28},
		{Interface: "Provider", RHS: "(*git.Provider)(nil)", Line: 30},
	}
	findings := verify(up, incomplete, nil)
	if len(findings) != 2 {
		t.Fatalf("verify(incomplete) findings = %v, want 2 findings", findings)
	}

	// Parse guards from a synthetic file.
	tmp := filepath.Join(t.TempDir(), "guards.go")
	src := `package vcs
import (
	"github.com/go-steer/core-vcs/providers/github"
	corevcs "github.com/go-steer/core-vcs/vcs"
)
var (
	_ corevcs.Provider = (*github.Provider)(nil)
)
`
	if err := os.WriteFile(tmp, []byte(src), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	parsed, err := parseGuards(tmp)
	if err != nil {
		t.Fatalf("parseGuards: %v", err)
	}
	if len(parsed) != 1 || parsed[0].Interface != "Provider" || !strings.Contains(parsed[0].RHS, "github.Provider") {
		t.Fatalf("parseGuards = %+v, want 1 Provider guard for github.Provider", parsed)
	}
}
