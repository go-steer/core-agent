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

package config

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestSave_PreservesUnknownTopLevelKeys is the core regression guard for
// #387: an older binary that loads → mutates → saves must NOT drop a
// top-level section written by a newer binary.
func TestSave_PreservesUnknownTopLevelKeys(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)

	// A file written by a "newer" binary carrying a section this build
	// doesn't know about, plus a section it does.
	orig := `{
  "version": 1,
  "permissions": {
    "allow": ["read_file"]
  },
  "future_feature": {
    "knob": 42,
    "nested": {"deep": true}
  }
}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Simulate the "always allow" TUI flow: append a permission.
	cfg.Permissions.Allow = append(cfg.Permissions.Allow, "write_file")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("saved file is not valid JSON: %v\n%s", err, raw)
	}
	// Unknown section survived, byte-for-byte in content.
	ff, ok := got["future_feature"]
	if !ok {
		t.Fatalf("future_feature section was dropped:\n%s", raw)
	}
	var ffObj struct {
		Knob   int `json:"knob"`
		Nested struct {
			Deep bool `json:"deep"`
		} `json:"nested"`
	}
	if err := json.Unmarshal(ff, &ffObj); err != nil {
		t.Fatalf("future_feature not preserved as-is: %v", err)
	}
	if ffObj.Knob != 42 || !ffObj.Nested.Deep {
		t.Errorf("future_feature content corrupted: %+v", ffObj)
	}
	// The mutation landed.
	if !strings.Contains(string(got["permissions"]), "write_file") {
		t.Errorf("permission mutation not persisted:\n%s", raw)
	}
}

// TestSave_DoesNotMaterializeDefaults verifies that saving a partial
// config keeps it partial — default sections the operator never set
// must not be pinned to disk, or a future default bump stops applying.
func TestSave_DoesNotMaterializeDefaults(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)

	// Operator's file only sets permissions; no model section.
	orig := `{"version":1,"permissions":{"allow":["read_file"]}}`
	if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	// Sanity: Load merged the default model into memory.
	if cfg.Model.Name == "" {
		t.Fatal("expected default model in memory")
	}
	cfg.Permissions.Deny = append(cfg.Permissions.Deny, "bash")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	if _, ok := got["model"]; ok {
		t.Errorf("default model section was materialized into a partial config:\n%s", raw)
	}
	if _, ok := got["agent"]; ok {
		t.Errorf("default agent section was materialized:\n%s", raw)
	}
	if _, ok := got["tool_output"]; ok {
		t.Errorf("default tool_output section was materialized:\n%s", raw)
	}
	// The file still loads and yields the current default model — i.e. a
	// future default bump would reach this operator.
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Model.Name != DefaultConfig().Model.Name {
		t.Errorf("model no longer tracks the default: got %q", reloaded.Model.Name)
	}
}

// TestSave_NewFileMode0600 verifies a freshly created config file is
// owner-only (the schema can hold api_key).
func TestSave_NewFileMode0600(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	cfg := DefaultConfig()
	cfg.Permissions.Allow = []string{"read_file"}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("new config file mode = %o, want 0600", got)
	}
}

// TestSave_RetainsExistingMode verifies Save does not silently re-chmod
// an existing file (the old 0644 hardcode bug).
func TestSave_RetainsExistingMode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	if err := os.WriteFile(path, []byte(`{"version":1,"permissions":{"allow":["x"]}}`), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	cfg.Permissions.Allow = append(cfg.Permissions.Allow, "y")
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Errorf("existing file mode = %o, want retained 0640", got)
	}
}

// TestSave_RoundTripReloads asserts the saved file is valid and reloads
// to an equivalent config (no corruption from the ordered emitter).
func TestSave_RoundTripReloads(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	body := `{"version":1,"model":{"name":"gemini-3-flash-preview","provider":"gemini"},"permissions":{"mode":"ask","allow":["a","b"]},"compaction":{"threshold":0.5}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if err := Save(path, cfg); err != nil {
		t.Fatalf("Save: %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if reloaded.Model.Name != "gemini-3-flash-preview" || reloaded.Model.Provider != "gemini" {
		t.Errorf("model not round-tripped: %+v", reloaded.Model)
	}
	if reloaded.Compaction.Threshold == nil || *reloaded.Compaction.Threshold != 0.5 {
		t.Errorf("compaction.threshold not round-tripped: %v", reloaded.Compaction.Threshold)
	}
}

// TestLoad_WarnsOnUnknownKey covers the security-relevant typo case: a
// misspelled key must not be silently ignored.
func TestLoad_WarnsOnUnknownKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ConfigFileName)
	// A mistyped top-level key ("denies" instead of nesting deny under
	// permissions) — the real gate never gets configured.
	body := `{"version":1,"model":{"name":"gemini-3.1-pro-preview"},"denies":["bash"]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	old := warnw
	warnw = &buf
	defer func() { warnw = old }()

	if _, err := Load(dir); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !strings.Contains(buf.String(), "denies") {
		t.Errorf("expected warning naming the mistyped key; got %q", buf.String())
	}
}

type unknownKeyTestCase struct {
	name      string
	body      string
	wantWarn  string
	wantError string
}

func runUnknownKeyCases(t *testing.T, cases []unknownKeyTestCase) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, ConfigFileName)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			var buf bytes.Buffer
			old := warnw
			warnw = &buf
			defer func() { warnw = old }()

			_, err := Load(dir)
			if tc.wantError != "" {
				if err == nil {
					t.Fatalf("expected fatal error containing %q, got nil (warn=%q)", tc.wantError, buf.String())
				}
				if !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.wantWarn == "" {
				if buf.Len() > 0 {
					t.Fatalf("expected no warnings, got %q", buf.String())
				}
				return
			}
			if !strings.Contains(buf.String(), tc.wantWarn) {
				t.Fatalf("warning %q does not contain %q", buf.String(), tc.wantWarn)
			}
		})
	}
}

func TestLoad_UnknownNestedKeysWarnOrFail(t *testing.T) {
	runUnknownKeyCases(t, []unknownKeyTestCase{
		{
			name:     "nested unknown under model warns",
			body:     `{"version":1,"model":{"name":"gemini-3-flash-preview","unknown_model_knob":true}}`,
			wantWarn: "model.unknown_model_knob",
		},
		{
			name:     "deeply nested unknown under model.vertex.context_cache warns",
			body:     `{"version":1,"model":{"name":"gemini-3-flash-preview","vertex":{"project":"p","location":"us-central1","context_cache":{"enabled":true,"unknown_cache_knob":1}}}}`,
			wantWarn: "model.vertex.context_cache.unknown_cache_knob",
		},
		{
			name:     "nested unknown inside slice element subagents[0] warns",
			body:     `{"version":1,"model":{"name":"gemini-3-flash-preview"},"subagents":[{"name":"helper","description":"d","instructions":"i","future_subagent_knob":true}]}`,
			wantWarn: "subagents[0].future_subagent_knob",
		},
		{
			name:     "nested unknown inside map of structs tool_output.per_tool warns",
			body:     `{"version":1,"model":{"name":"gemini-3-flash-preview"},"tool_output":{"per_tool":{"bash":{"max_bytes":1024,"extra_knob":5}}}}`,
			wantWarn: "tool_output.per_tool.bash.extra_knob",
		},
		{
			name:     "nested unknown inside map of slices of structs hooks warns",
			body:     `{"version":1,"model":{"name":"gemini-3-flash-preview"},"hooks":{"tool-start":[{"command":"echo ok","extra_hook_field":true}]}}`,
			wantWarn: "hooks.tool-start[0].extra_hook_field",
		},
		{
			name:     "dynamic map keys with scalar values do not warn",
			body:     `{"version":1,"model":{"name":"gemini-3-flash-preview"},"compaction":{"threshold_by_tier":{"small":0.3}},"url_scope":{"headers":{"example.com":{"X-Test":"1"}}}}`,
			wantWarn: "",
		},
		{
			name:      "unknown key under permissions is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"permissions":{"denny":["bash(rm *)"]}}`,
			wantError: "permissions.denny",
		},
		{
			name:      "unknown key under permissions.auto is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"permissions":{"auto":{"elligible":["bash"]}}}`,
			wantError: "permissions.auto.elligible",
		},
		{
			name:      "mistyped agent max_sesion_cost_usd is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"agent":{"max_sesion_cost_usd":1}}`,
			wantError: "agent.max_sesion_cost_usd",
		},
		{
			name:      "mistyped agent max_trun_cost_usd is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"agent":{"max_trun_cost_usd":1}}`,
			wantError: "agent.max_trun_cost_usd",
		},
		{
			name:      "unknown key under subagents[0].budgets is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"subagents":[{"name":"helper","description":"d","instructions":"i","budgets":{"max_cots_usd":2}}]}`,
			wantError: "subagents[0].budgets.max_cots_usd",
		},
		{
			name:      "mistyped subagents[0].budget container is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"subagents":[{"name":"helper","description":"d","instructions":"i","budget":{"max_cost_usd":2}}]}`,
			wantError: "subagents[0].budget",
		},
		{
			name:      "unknown key under model.pricing.<model> is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview","pricing":{"gemini-3-flash-preview":{"input_per_mtok":0.1,"output_per_mtok":0.2,"unknown_rate":0.3}}}}`,
			wantError: "model.pricing.gemini-3-flash-preview.unknown_rate",
		},
		{
			name:      "unknown key under attach.cost_rate_limit is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"attach":{"cost_rate_limit":{"per_minute":1,"brust":2}}}`,
			wantError: "attach.cost_rate_limit.brust",
		},
		{
			name:      "mistyped top-level permission container is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"permission":{"deny":["bash(rm *)"]}}`,
			wantError: "permission",
		},
		{
			name:      "mistyped model.pricng container is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview","pricng":{"gemini-3-flash-preview":{"input_per_mtok":0.1}}}}`,
			wantError: "model.pricng",
		},
		{
			name:      "unknown outer container wrapping fatal nested cost key is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"agents":{"max_session_cost_usd":1}}`,
			wantError: "agents.max_session_cost_usd",
		},
		{
			name:      "duplicate permissions block does not hide fatal typo in first occurrence",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"permissions":{"denny":["bash(rm *)"]},"permissions":{"mode":"ask"}}`,
			wantError: "permissions.denny",
		},
		{
			name:      "non-ASCII character in budget key still matches edit distance",
			body:      "{\"version\":1,\"model\":{\"name\":\"gemini-3-flash-preview\"},\"subagents\":[{\"name\":\"helper\",\"description\":\"d\",\"instructions\":\"i\",\"budgets\\u200b\":{\"max_cost_usd\":2}}]}",
			wantError: "subagents[0].budgets",
		},
	})
}

func TestLoad_UnknownSafetyAndScopeKeysAreFatal(t *testing.T) {
	runUnknownKeyCases(t, []unknownKeyTestCase{
		{
			name:      "unknown key under safety is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"safety":{"watchdgo":"enforce"}}`,
			wantError: "safety.watchdgo",
		},
		{
			name:      "mistyped top-level safety container is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"safey":{"watchdog":"enforce"}}`,
			wantError: "safey",
		},
		{
			name:      "unknown key under url_scope is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"url_scope":{"allow":["*"],"denny":["evil.com"]}}`,
			wantError: "url_scope.denny",
		},
		{
			name:      "unknown key under path_scope is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"path_scope":{"allow_path":[{"path":"/tmp","access":"r"}]}}`,
			wantError: "path_scope.allow_path",
		},
		{
			name:      "attach read_only typo is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"attach":{"read_only":true}}`,
			wantError: "attach.read_only",
		},
		{
			name:      "unknown key under attach.multi_session.auth is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"attach":{"multi_session":{"auth":{"tabel_file":"/etc/users.json"}}}}`,
			wantError: "attach.multi_session.auth.tabel_file",
		},
		{
			name:      "unknown key under attach.multi_session is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"attach":{"multi_session":{"allow_anon":false}}}`,
			wantError: "attach.multi_session.allow_anon",
		},
		{
			name:      "unknown key under model.builtin_tools is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview","builtin_tools":{"websearch":false}}}`,
			wantError: "model.builtin_tools.websearch",
		},
		{
			name:      "unknown key under subagents[0].model.builtin_tools is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"subagents":[{"name":"helper","description":"d","instructions":"i","model":{"name":"gemini-3-flash-preview","builtin_tools":{"urlcontext":false}}}]}`,
			wantError: "subagents[0].model.builtin_tools.urlcontext",
		},
		{
			name:      "mistyped tools.disabled is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"tools":{"disabled":["bash"]}}`,
			wantError: "tools.disabled",
		},
		{
			name:      "mistyped subagents[0].tool allowlist is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"subagents":[{"name":"helper","description":"d","instructions":"i","tool":["read_file"]}]}`,
			wantError: "subagents[0].tool",
		},
		{
			name:      "unknown key under subagents[0].remote.auth is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"subagents":[{"name":"helper","description":"d","remote":{"endpoint":"https://example.com/rpc","auth":{"type":"bearer","token_file":"/tmp/tok"}}}]}`,
			wantError: "subagents[0].remote.auth.token_file",
		},
		{
			name:      "case-insensitive struct container still catches nested fatal key",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"Attach":{"Cost_Rate_Limit":{"per_minute":1,"brust":2}}}`,
			wantError: "attach.cost_rate_limit.brust",
		},
		{
			name:      "case-insensitive Permissions container still catches nested fatal key",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"Permissions":{"denny":["bash(rm *)"]}}`,
			wantError: "permissions.denny",
		},
		{
			name:      "camelCase CostRateLimit typo under attach is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"attach":{"CostRateLimit":{"per_minute":1}}}`,
			wantError: "attach.CostRateLimit",
		},
		{
			name:     "valid mixed-case Providers block does not emit false unknown-key warning",
			body:     `{"version":1,"model":{"name":"gemini-3-flash-preview"},"Providers":[{"name":"local","extends":"ollama"}]}`,
			wantWarn: "",
		},
		{
			name:      "camelCase USD/MTok suffix still matches word boundary",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview","inputPerMTok":0.1}}`,
			wantError: "model.inputPerMTok",
		},
		{
			name:     "statusd substring containing usd only warns",
			body:     `{"version":1,"model":{"name":"gemini-3-flash-preview"},"ui":{"statusd":true}}`,
			wantWarn: "ui.statusd",
		},
		{
			name:      "duplicate Providers + providers blocks still validate first block strictly",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"Providers":[{"name":"local","extends":"ollama","open_modles":false}],"providers":[{"name":"local","extends":"ollama"}]}`,
			wantError: "open_modles",
		},
		{
			name:      "mistyped top-level tool container with disable is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"tool":{"disable":["bash"]}}`,
			wantError: "tool",
		},
		{
			name:      "unknown key under subagents[0].remote is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"subagents":[{"name":"helper","description":"d","remote":{"endpoint":"https://example.com/rpc","auht":{"type":"bearer","token_env":"TOK"}}}]}`,
			wantError: "subagents[0].remote.auht",
		},
		{
			name:      "mistyped subagents[0].mcp_servers is fatal",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"subagents":[{"name":"helper","description":"d","instructions":"i","mcp_servers":[]}]}`,
			wantError: "subagents[0].mcp_servers",
		},
		{
			name:      "acronym USD before TitleCase word matches word boundary",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview"},"agent":{"maxUSDPerTurn":1}}`,
			wantError: "agent.maxUSDPerTurn",
		},
		{
			name:      "digit before MTok matches word boundary",
			body:      `{"version":1,"model":{"name":"gemini-3-flash-preview","pricePer1MTok":0.5}}`,
			wantError: "model.pricePer1MTok",
		},
		{
			name:     "nested provider field in non-critical block only warns",
			body:     `{"version":1,"model":{"name":"gemini-3-flash-preview"},"compaction":{"provider":"gemini"}}`,
			wantWarn: "compaction.provider",
		},
	})
}

func TestSave_MixedCaseTopLevelKeysCanonicalizeOnRoundTrip(t *testing.T) {
	t.Parallel()

	t.Run("resetting mixed-case UI section back to default persists the reset", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ConfigFileName)
		orig := `{"version":1,"model":{"name":"gemini-3-flash-preview"},"UI":{"theme":"dark"}}`
		if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}

		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		if cfg.UI.Theme != ThemeDark {
			t.Fatalf("loaded UI.Theme = %q, want %q", cfg.UI.Theme, ThemeDark)
		}

		cfg.UI.Theme = ThemeAuto
		if err := Save(path, cfg); err != nil {
			t.Fatalf("Save: %v", err)
		}

		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(saved), `"UI"`) {
			t.Errorf("Save left non-canonical \"UI\" block on disk:\n%s", saved)
		}

		reloaded, err := Load(dir)
		if err != nil {
			t.Fatalf("reload Load: %v", err)
		}
		if reloaded.UI.Theme != ThemeAuto {
			t.Errorf("reloaded UI.Theme = %q, want %q (reset to default was lost)", reloaded.UI.Theme, ThemeAuto)
		}
	})

	t.Run("mutating mixed-case Permissions section replaces in place without duplicate block", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, ConfigFileName)
		orig := `{"version":1,"model":{"name":"gemini-3-flash-preview"},"Permissions":{"mode":"yolo","use_builtin_allow":false}}`
		if err := os.WriteFile(path, []byte(orig), 0o600); err != nil {
			t.Fatal(err)
		}

		cfg, err := Load(dir)
		if err != nil {
			t.Fatalf("Load: %v", err)
		}
		cfg.Permissions.Mode = "ask"
		cfg.Permissions.UseBuiltinAllow = nil
		cfg.Permissions.Allow = []string{"read_file"}

		if err := Save(path, cfg); err != nil {
			t.Fatalf("Save: %v", err)
		}

		saved, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(saved), `"Permissions"`) {
			t.Errorf("Save left duplicate/mixed-case \"Permissions\" block on disk:\n%s", saved)
		}

		reloaded, err := Load(dir)
		if err != nil {
			t.Fatalf("reload Load: %v", err)
		}
		if reloaded.Permissions.Mode != "ask" {
			t.Errorf("reloaded Permissions.Mode = %q, want %q", reloaded.Permissions.Mode, "ask")
		}
		if reloaded.Permissions.UseBuiltinAllow != nil {
			t.Errorf("reloaded Permissions.UseBuiltinAllow = %v, want nil (stale mixed-case block merged back)", *reloaded.Permissions.UseBuiltinAllow)
		}
	})
}
