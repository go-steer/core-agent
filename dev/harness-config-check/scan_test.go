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
	"strings"
	"testing"
)

func TestPinRecognition(t *testing.T) {
	cases := []struct {
		name   string
		script string
		pinned bool
	}{
		{"separate value", `"${CORE_AGENT}" -c /tmp/a/.agents/config.json -p hi`, true},
		{"equals form", `"${CORE_AGENT}" -c=/tmp/a/.agents/config.json -p hi`, true},
		{"double dash", `"${CORE_AGENT}" --c /tmp/a/.agents/config.json -p hi`, true},
		{"double dash equals", `"${CORE_AGENT}" --c=/tmp/a/.agents/config.json -p hi`, true},
		{"quoted value", `"${CORE_AGENT}" -c "${WORK_DIR}/.agents/config.json" -p hi`, true},
		{"pin after other flags", `"${CORE_AGENT}" --provider=echo --yolo -c /tmp/c.json`, true},
		{"nothing", `"${CORE_AGENT}" --provider=echo -p hi`, false},

		// --agents-dir is NOT a pin. loadConfig() reads config.json by
		// walking up from cwd before resolveAgentsDir() applies the
		// flag, so this leaves the model, permissions and budgets coming
		// from whatever .agents/ is above the script's cwd. Verified
		// live: with --agents-dir pointing at an empty tree the startup
		// summary still read "config: source=<repo>/.agents/config.json".
		{"agents-dir is not a pin", `"${CORE_AGENT}" --agents-dir /tmp/a/.agents -p hi`, false},
		{"agents-dir plus -c is", `"${CORE_AGENT}" --agents-dir /tmp/a/.agents -c /tmp/a/c.json`, true},

		// Long flags that merely start with the same letters.
		{"compaction-threshold", `"${CORE_AGENT}" --compaction-threshold=0.5 -p hi`, false},
		{"checkpoint", `"${CORE_AGENT}" --checkpoint=operator -p hi`, false},
		{"color", `"${CORE_AGENT}" --color=never -p hi`, false},

		// A pin belonging to the NEXT command does not count.
		{"pin after a separator", `"${CORE_AGENT}" -p hi; other -c /tmp/c.json`, false},
		{"pin in the next pipeline stage", `"${CORE_AGENT}" -p hi | other -c /tmp/c.json`, false},

		// The value has to look like a config file. Without this, a -c
		// that is really the VALUE of the preceding flag reads as a pin.
		{"c as another flag's value", `"${CORE_AGENT}" --log-file -c`, false},
		{"c with a flag after it", `"${CORE_AGENT}" -c --yolo -p hi`, false},
		{"c at end of line", `"${CORE_AGENT}" -p hi -c`, false},
		{"c after a bare --", `"${CORE_AGENT}" -p hi -- -c`, false},
		{"empty equals form", `"${CORE_AGENT}" -c= -p hi`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scan("t.sh", tc.script)
			if len(got) != 1 {
				t.Fatalf("expected exactly 1 invocation, got %d: %+v", len(got), got)
			}
			if got[0].Pinned != tc.pinned {
				t.Errorf("Pinned = %v, want %v (args: %s)", got[0].Pinned, tc.pinned, got[0].Args)
			}
		})
	}
}

// Inside a dispatched command string the lexer splits on whitespace
// without tracking quotes, so the words of an argument become tokens of
// their own. Crediting a `-c` among them marks the site pinned forever —
// and these are the two sites (dev/uat/attach/run.sh) the whole check
// was built for, so this is the miss that would matter most.
func TestNestedPinsAreNotCreditedFromInsideAnArgument(t *testing.T) {
	cases := []struct {
		name   string
		script string
		pinned bool
	}{
		{
			"-c inside a quoted prompt",
			`cmd="cd /tmp && '${BIN}' --provider=echo -p 'explain the -c flag'"`,
			false,
		},
		{
			"-c and a json word inside a quoted prompt",
			`cmd="cd /tmp && '${BIN}' -p 'pass -c config.json to it'"`,
			false,
		},
		{
			"a real nested pin still counts",
			`cmd="cd /tmp && '${BIN}' -c '${dir}/.agents/config.json' -p hi"`,
			true,
		},
		{
			"a real nested pin with a literal path",
			`cmd="cd /tmp && '${BIN}' -c /tmp/x/.agents/config.json -p hi"`,
			true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scan("t.sh", tc.script)
			if len(got) != 1 {
				t.Fatalf("expected exactly 1 invocation, got %d: %+v", len(got), got)
			}
			if got[0].Pinned != tc.pinned {
				t.Errorf("Pinned = %v, want %v (args: %s)", got[0].Pinned, tc.pinned, got[0].Args)
			}
		})
	}
}

// Wrappers that run the command named in their own arguments. Each of
// these executes the binary under real bash; `nice` and `stdbuf` are
// asserted here rather than in the oracle because the oracle would fail
// on a machine without coreutils rather than skip.
func TestWrappersAndGoRunAreInvocations(t *testing.T) {
	for _, script := range []string{
		`nice -n 5 "${CORE_AGENT}" -p hi`,
		`ionice -c3 "${CORE_AGENT}" -p hi`,
		`stdbuf -o0 "${CORE_AGENT}" -p hi`,
		`setsid "${CORE_AGENT}" -p hi`,
		`timeout 30s nice -n 5 "${CORE_AGENT}" -p hi`,
		// The wrapper's own argument as a variable rather than a literal.
		`timeout "${TIMEOUT_SECS}" "${CORE_AGENT}" -p hi`,
		`timeout -k "${GRACE}" "${LIMIT}" "${AGENT_BIN}" -p hi`,
		// The unbraced spelling of the same thing.
		`timeout $SECS "${CORE_AGENT}" -p hi`,
		`timeout "$SECS" "${CORE_AGENT}" -p hi`,
		`go run ./cmd/core-agent -p hi`,
		`go run "${REPO_ROOT}/cmd/core-agent" -p hi`,
	} {
		got := Scan("t.sh", script)
		if len(got) != 1 {
			t.Errorf("%s: got %d invocations, want 1: %+v", script, len(got), got)
			continue
		}
		if got[0].Pinned || got[0].Exempt != "" {
			t.Errorf("%s: want an unpinned finding, got %+v", script, got[0])
		}
	}
	// ...and `go run` of something else is not one.
	for _, script := range []string{
		`go run ./dev/smoke/cmd/inspect-grounding "${db}"`,
		`go run ./dev/harness-config-check --print`,
		`go build -o "${CORE_AGENT}" ./cmd/core-agent`,
	} {
		if got := Scan("t.sh", script); len(got) != 0 {
			t.Errorf("false positive on %s: %+v", script, got)
		}
	}
}

func TestSubcommandExemptions(t *testing.T) {
	cases := []struct {
		name       string
		script     string
		wantExempt bool
	}{
		// attach and ls are peeled off in main() before flag.Parse, read
		// no config, and reject -c outright:
		//   $ core-agent attach -c x URL
		//   flag provided but not defined: -c
		{"attach", `"${BIN}" attach --token-env=ATTACH_TOKEN "${url}"`, true},
		{"ls", `"${BIN}" ls --token-env=ATTACH_TOKEN "${url}"`, true},
		{"version", `"${BIN}" --version`, true},

		// Only as the FIRST argument — `-p "attach the volume"` is not a
		// subcommand, and the dispatch in main() only looks at os.Args[1].
		{"attach as a later word", `"${BIN}" -p "attach the volume"`, false},
		{"ls as a later word", `"${BIN}" --provider=echo ls`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := Scan("t.sh", tc.script)
			if len(got) != 1 {
				t.Fatalf("expected exactly 1 invocation, got %d: %+v", len(got), got)
			}
			if (got[0].Exempt != "") != tc.wantExempt {
				t.Errorf("Exempt = %q, want exempt=%v", got[0].Exempt, tc.wantExempt)
			}
		})
	}
}

// A finding that points at the wrong line is a finding the reader cannot
// act on, and the nested sites — a command line built as a string and
// dispatched through tmux — are exactly the ones where an offset is easy
// to get wrong.
func TestFindingLineNumbers(t *testing.T) {
	script := strings.Join([]string{
		`#!/usr/bin/env bash`,            // 1
		`set -euo pipefail`,              // 2
		``,                               // 3
		`"${CORE_AGENT}" -p hi`,          // 4
		``,                               // 5
		`spawn() {`,                      // 6
		`    local cmd="cd /tmp && \`,    // 7
		`        '${BIN}' -p hi"`,        // 8
		`    tmux_new_window x "${cmd}"`, // 9
		`}`,                              // 10
	}, "\n")
	got := Scan("t.sh", script)
	if len(got) != 2 {
		t.Fatalf("expected 2 invocations, got %d: %+v", len(got), got)
	}
	if got[0].Line != 4 {
		t.Errorf("top-level invocation on line %d, want 4", got[0].Line)
	}
	if got[1].Line != 8 {
		t.Errorf("dispatched invocation on line %d, want 8", got[1].Line)
	}
}

// The scanner must not report the same site twice: a bare "${CORE_AGENT}"
// is both a token at the outer level and a quoted span, and counting it
// in both places would put a phantom in every report.
func TestNoDoubleCounting(t *testing.T) {
	for _, script := range []string{
		`"${CORE_AGENT}" -p hi`,
		`'${CORE_AGENT}' -p hi`,
		`${CORE_AGENT} -p hi`,
		`"${CORE_AGENT} -p hi"`, // whole command quoted: seen once, nested
	} {
		got := Scan("t.sh", script)
		if len(got) != 1 {
			t.Errorf("%s: got %d invocations, want 1: %+v", script, len(got), got)
		}
	}
}

// The repo's own harness is the corpus a hand-written false-positive
// suite cannot match. These are the shapes that actually appear in
// dev/**, each of which mentions the binary without running it.
func TestRealHarnessShapesAreNotInvocations(t *testing.T) {
	for _, script := range []string{
		`CORE_AGENT="${CORE_AGENT:-/tmp/core-agent}"`,
		`(cd "${repo_root}" && go build -o "${CORE_AGENT}" ./cmd/core-agent)`,
		`if [[ ! -x "${CORE_AGENT}" || "${1:-}" == "--force" ]]; then :; fi`,
		`if [[ ! -x "${BIN}" ]] || [[ "${REPO_ROOT}/cmd/core-agent/main.go" -nt "${BIN}" ]]; then :; fi`,
		`log_step "building core-agent → ${CORE_AGENT}"`,
		`log "binary at ${BIN}"`,
		`log "core-agent attach ${url}"`,
		`go run ./dev/smoke/cmd/evalrun --binary "${CORE_AGENT}" --case x`,
		`drill_die "deploy/core-agent in ${DEMO_NS} has no ready replica."`,
		`pkill -f 'port-forward svc/core-agent ${DRILL_PORT}:7777'`,
		`ATTACH_TOKEN="${ATTACH_TOKEN}" "${TUI_BIN}" --token-env=ATTACH_TOKEN "${url}"`,
		`DRILL_APP="${DRILL_APP:-core-agent}"`,
	} {
		if got := Scan("t.sh", script); len(got) != 0 {
			t.Errorf("false positive on %s: %+v", script, got)
		}
	}
}
