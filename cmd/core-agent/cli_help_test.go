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
	"flag"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/taskclass"
)

func TestResolvePermissionModeFlag(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		initMode string
		yolo     bool
		flagMode string
		wantMode string
		wantYolo bool
		wantErr  bool
		wantDep  bool
	}{
		{
			name:     "unset preserves config mode",
			initMode: config.PermissionModeAsk,
			wantMode: config.PermissionModeAsk,
		},
		{
			name:     "explicit acceptEdits overrides config",
			initMode: config.PermissionModeAsk,
			flagMode: config.PermissionModeAcceptEdits,
			wantMode: config.PermissionModeAcceptEdits,
		},
		{
			name:     "explicit auto overrides config",
			initMode: config.PermissionModeAsk,
			flagMode: config.PermissionModeAuto,
			wantMode: config.PermissionModeAuto,
		},
		{
			name:     "explicit plan overrides config",
			initMode: config.PermissionModeAsk,
			flagMode: config.PermissionModePlan,
			wantMode: config.PermissionModePlan,
		},
		{
			name:     "explicit allow overrides config",
			initMode: config.PermissionModeAsk,
			flagMode: config.PermissionModeAllow,
			wantMode: config.PermissionModeAllow,
		},
		{
			name:     "explicit yolo sets yolo bool too",
			initMode: config.PermissionModeAsk,
			flagMode: config.PermissionModeYolo,
			wantMode: config.PermissionModeYolo,
			wantYolo: true,
		},
		{
			name:     "deprecated accept-edits normalizes to acceptEdits and warns",
			initMode: config.PermissionModeAsk,
			flagMode: "accept-edits",
			wantMode: config.PermissionModeAcceptEdits,
			wantDep:  true,
		},
		{
			name:     "yolo flag alone sets mode yolo",
			initMode: config.PermissionModeAsk,
			yolo:     true,
			wantMode: config.PermissionModeYolo,
			wantYolo: true,
		},
		{
			name:     "yolo flag and permission-mode=yolo agree",
			initMode: config.PermissionModeAsk,
			yolo:     true,
			flagMode: config.PermissionModeYolo,
			wantMode: config.PermissionModeYolo,
			wantYolo: true,
		},
		{
			name:     "contradicting --permission-mode and --yolo fails",
			initMode: config.PermissionModeAsk,
			yolo:     true,
			flagMode: config.PermissionModeAsk,
			wantErr:  true,
		},
		{
			name:     "unknown mode fails",
			initMode: config.PermissionModeAsk,
			flagMode: "super-yolo",
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			mode := tc.initMode
			yolo := tc.yolo
			var notices []string
			err := resolvePermissionModeFlag(&mode, &yolo, tc.flagMode, func(s string) {
				notices = append(notices, s)
			})
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil (mode=%q, yolo=%v)", mode, yolo)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if mode != tc.wantMode || yolo != tc.wantYolo {
				t.Errorf("got (mode=%q, yolo=%v), want (%q, %v)", mode, yolo, tc.wantMode, tc.wantYolo)
			}
			if tc.wantDep && len(notices) == 0 {
				t.Error("expected deprecation notice for accept-edits, got none")
			}
			if !tc.wantDep && len(notices) > 0 {
				t.Errorf("unexpected notice: %v", notices)
			}
		})
	}
}

func TestWriteCLIUsage_GroupsSubcommandsAndCoversEveryMainFlag(t *testing.T) {
	t.Parallel()

	covered := make(map[string]bool)
	for _, g := range cliFlagGroups {
		for _, f := range g.Flags {
			for _, name := range f.Names {
				covered[name] = true
			}
		}
	}

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "main.go", nil, 0)
	if err != nil {
		t.Fatalf("parse main.go: %v", err)
	}
	registered := make(map[string]bool)
	fs := flag.NewFlagSet("core-agent", flag.ContinueOnError)
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		ident, ok := sel.X.(*ast.Ident)
		if !ok || ident.Name != "flag" {
			return true
		}
		nameArgIdx := -1
		switch sel.Sel.Name {
		case "String", "Bool", "Int", "Float64", "Duration", "Func":
			nameArgIdx = 0
		case "StringVar", "BoolVar", "IntVar", "Float64Var", "DurationVar":
			nameArgIdx = 1
		}
		if nameArgIdx < 0 || nameArgIdx >= len(call.Args) {
			return true
		}
		lit, ok := call.Args[nameArgIdx].(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		flagName, err := strconv.Unquote(lit.Value)
		if err != nil {
			return true
		}
		registered[flagName] = true
		if fs.Lookup(flagName) == nil {
			fs.String(flagName, "", "test flag "+flagName)
		}
		if !covered[flagName] {
			t.Errorf("flag %q registered in main.go is not listed in cliFlagGroups (cmd/core-agent/cli_help.go)", flagName)
		}
		return true
	})

	for _, g := range cliFlagGroups {
		for _, f := range g.Flags {
			if f.Optional {
				continue
			}
			for _, name := range f.Names {
				if !registered[name] {
					t.Errorf("flag %q in cliFlagGroups (%s) is not registered in main.go", name, g.Title)
				}
			}
		}
	}

	var buf bytes.Buffer
	writeCLIUsage(&buf, fs)
	out := buf.String()

	for _, want := range []string{
		"Subcommands:",
		"attach <url>",
		"ls <url>",
		"auth hash-token",
		"config init|show|validate",
		"trust [list|revoke <dir>]",
		"--version",
		"--permission-mode <mode>",
		"Session & Prompt:",
		"Model & Caching:",
		"Permissions & Safety:",
		"Tools, Subagents & Context:",
		"Remote Attach & Fleet:",
		"Observability & Agent Card:",
		"Deprecated:",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("writeCLIUsage output missing %q", want)
		}
	}
	if strings.Contains(out, "Other Flags:") {
		t.Errorf("writeCLIUsage emitted Other Flags section for main.go flags:\n%s", out)
	}

	// Verify Other Flags fallback when a new flag is registered on FlagSet.
	fs.String("unlisted-exp-flag", "", "experimental test flag\nsecond line")
	buf.Reset()
	writeCLIUsage(&buf, fs)
	if !strings.Contains(buf.String(), "Other Flags:") || !strings.Contains(buf.String(), "--unlisted-exp-flag") {
		t.Errorf("expected Other Flags fallback for unlisted flag, got:\n%s", buf.String())
	}
}

func TestApplyPlanMode_EmitsDeprecationNoticeWhenPlanFirstSet(t *testing.T) {
	t.Parallel()
	cfg := config.DefaultConfig()
	var notices []string
	err := applyPlanMode(cfg, t.TempDir(), false, "", toolProfileOpts{
		planFirst:    true,
		planFirstSet: true,
	}, taskclass.Profile{}, func(s string) {
		notices = append(notices, s)
	})
	if err != nil {
		t.Fatalf("applyPlanMode: %v", err)
	}
	found := false
	for _, n := range notices {
		if n == planFirstDeprecationNotice {
			found = true
		}
	}
	if !found {
		t.Errorf("expected %q in notices, got %v", planFirstDeprecationNotice, notices)
	}

	// Without planFirstSet, no deprecation notice is emitted and a nil send callback does not panic.
	notices = nil
	if err := applyPlanMode(config.DefaultConfig(), t.TempDir(), false, "", toolProfileOpts{planMode: config.PlanModeRequired}, taskclass.Profile{}, nil); err != nil {
		t.Fatalf("applyPlanMode with nil send: %v", err)
	}
}

func TestResolveSandboxFlags(t *testing.T) {
	t.Parallel()
	var sb config.SandboxConfig
	if err := resolveSandboxFlags(&sb, "bwrap", false); err != nil {
		t.Fatalf("resolveSandboxFlags(bwrap): %v", err)
	}
	if sb.Mode != config.SandboxModeEnforce || sb.Driver != config.SandboxDriverBwrap {
		t.Errorf("bwrap got (%q, %q), want (enforce, bwrap)", sb.Mode, sb.Driver)
	}

	if err := resolveSandboxFlags(&sb, "", true); err != nil {
		t.Fatalf("resolveSandboxFlags(noSandbox): %v", err)
	}
	if sb.Mode != config.SandboxModeOff {
		t.Errorf("noSandbox got mode %q, want off", sb.Mode)
	}

	if err := resolveSandboxFlags(&sb, "enforce", true); err == nil {
		t.Error("expected error when combining --no-sandbox and --sandbox=enforce")
	}

	if err := resolveSandboxFlags(&sb, "bogus", false); err == nil {
		t.Error("expected error for unknown --sandbox value")
	}

	// Case-insensitive flag and driver reset on --sandbox=auto / --sandbox=enforce from policy.
	sb = config.SandboxConfig{Driver: config.SandboxDriverPolicy}
	if err := resolveSandboxFlags(&sb, "ENFORCE", false); err != nil {
		t.Fatalf("resolveSandboxFlags(ENFORCE): %v", err)
	}
	if sb.Mode != config.SandboxModeEnforce || sb.Driver != config.SandboxDriverAuto {
		t.Errorf("ENFORCE got (%q, %q), want (enforce, auto)", sb.Mode, sb.Driver)
	}
	sb = config.SandboxConfig{Driver: config.SandboxDriverSeatbelt}
	if err := resolveSandboxFlags(&sb, "AUTO", false); err != nil {
		t.Fatalf("resolveSandboxFlags(AUTO): %v", err)
	}
	if sb.Mode != config.SandboxModeAuto || sb.Driver != config.SandboxDriverAuto {
		t.Errorf("AUTO got (%q, %q), want (auto, auto)", sb.Mode, sb.Driver)
	}

	// --sandbox=container without container_image fails at flag validation.
	sbNoImg := config.SandboxConfig{}
	if err := resolveSandboxFlags(&sbNoImg, "container", false); err == nil {
		t.Error("expected error for --sandbox=container when container_image is empty")
	}
	sbWithImg := config.SandboxConfig{ContainerImage: "alpine:3.20"}
	if err := resolveSandboxFlags(&sbWithImg, "container", false); err != nil {
		t.Errorf("unexpected error for --sandbox=container with image: %v", err)
	}
}
