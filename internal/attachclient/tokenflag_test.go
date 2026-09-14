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

package attachclient

import (
	"bytes"
	"strings"
	"testing"
)

// No t.Parallel anywhere in this file: t.Setenv forbids it, and every
// interesting case here is about what an env var does or does not hold.

// The reported bug (#947), which produced a bare 401 and no explanation.
// The operator types the shape a careful person types — quoted, expanded
// by the shell — and the binary looks the secret up as a variable name.
func TestResolveTokenEnv_TheExpandedSecretIsDiagnosed(t *testing.T) {
	var warn bytes.Buffer
	got := ResolveTokenEnv("core-agent-tui", "sk-live-9f3c2a7e1b4d", "", &warn)

	if got != "" {
		t.Errorf("token = %q, want empty — a secret is not an env-var name", got)
	}
	if !strings.Contains(warn.String(), "NAME of an env var") {
		t.Errorf("no diagnosis for a value that cannot be an env-var name; stderr was:\n%s", warn.String())
	}
	// The whole point of the indirection is that the secret does not get
	// written anywhere. A diagnostic that echoes it turns a 401 into a
	// leak, in the scrollback and in whatever ships stderr off the box.
	if strings.Contains(warn.String(), "sk-live-9f3c2a7e1b4d") {
		t.Errorf("the warning echoed the secret back:\n%s", warn.String())
	}
}

// The other silent 401, and the likelier one in a container: the name is
// perfectly well-formed and the variable was simply never plumbed in.
func TestResolveTokenEnv_UnsetVariableIsDiagnosed(t *testing.T) {
	var warn bytes.Buffer
	got := ResolveTokenEnv("core-agent attach", "ATTACH_TOKEN_NOT_SET_947", "", &warn)

	if got != "" {
		t.Errorf("token = %q, want empty", got)
	}
	if !strings.Contains(warn.String(), "unset or empty") {
		t.Errorf("no diagnosis for a name that resolves to nothing; stderr was:\n%s", warn.String())
	}
	if !strings.Contains(warn.String(), "401") {
		t.Errorf("the warning does not connect the flag to the 401 the operator will see:\n%s", warn.String())
	}
}

func TestResolveTokenEnv_ResolvesAndSaysNothing(t *testing.T) {
	t.Setenv("ATTACH_TOKEN", "s3cret")
	var warn bytes.Buffer

	if got := ResolveTokenEnv("core-agent-tui", "ATTACH_TOKEN", "", &warn); got != "s3cret" {
		t.Errorf("token = %q, want s3cret", got)
	}
	if warn.Len() != 0 {
		t.Errorf("the working configuration printed a warning:\n%s", warn.String())
	}
}

// The deprecated flag keeps working with unchanged semantics. A rename
// that broke every existing invocation would be a worse bug than the one
// being fixed.
func TestResolveTokenEnv_LegacyFlagStillWorksAndWarns(t *testing.T) {
	t.Setenv("ATTACH_TOKEN", "s3cret")
	var warn bytes.Buffer

	if got := ResolveTokenEnv("core-agent-tui", "", "ATTACH_TOKEN", &warn); got != "s3cret" {
		t.Errorf("token = %q, want s3cret — --token must keep resolving", got)
	}
	if !strings.Contains(warn.String(), "deprecated") {
		t.Errorf("--token did not announce its deprecation:\n%s", warn.String())
	}
	if !strings.Contains(warn.String(), "--token-env") {
		t.Errorf("the deprecation does not name the replacement:\n%s", warn.String())
	}
}

// Both given. Which one won has to be stated, or the operator spends the
// incident guessing.
func TestResolveTokenEnv_BothFlagsPrefersTokenEnvAndSaysSo(t *testing.T) {
	t.Setenv("NEW_TOKEN", "new")
	t.Setenv("OLD_TOKEN", "old")
	var warn bytes.Buffer

	if got := ResolveTokenEnv("core-agent ls", "NEW_TOKEN", "OLD_TOKEN", &warn); got != "new" {
		t.Errorf("token = %q, want new — --token-env is the flag that still exists", got)
	}
	if !strings.Contains(warn.String(), "ignoring --token") {
		t.Errorf("the binary did not say which flag it read:\n%s", warn.String())
	}
}

// No token is a supported posture — Unix-socket attach, and a daemon
// started without --attach-token. It must resolve silently, or every
// such run grows a spurious warning.
func TestResolveTokenEnv_NoFlagsIsSilent(t *testing.T) {
	var warn bytes.Buffer

	if got := ResolveTokenEnv("core-agent attach", "", "", &warn); got != "" {
		t.Errorf("token = %q, want empty", got)
	}
	if warn.Len() != 0 {
		t.Errorf("an unauthenticated attach printed a warning:\n%s", warn.String())
	}
}

// A caller with nowhere to print still gets the right answer.
func TestResolveTokenEnv_NilWriterDoesNotPanic(t *testing.T) {
	t.Setenv("ATTACH_TOKEN", "s3cret")

	if got := ResolveTokenEnv("core-agent-tui", "ATTACH_TOKEN", "", nil); got != "s3cret" {
		t.Errorf("token = %q, want s3cret", got)
	}
	if got := ResolveTokenEnv("core-agent-tui", "not a name", "", nil); got != "" {
		t.Errorf("token = %q, want empty", got)
	}
}

func TestLooksLikeEnvName(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
		why  string
	}{
		{"ATTACH_TOKEN", true, "the ordinary case"},
		{"attach_token", true, "lowercase env vars are unusual but legal, and calling one a secret would be a confident wrong answer"},
		{"_LEADING", true, "underscore may lead"},
		{"T0KEN9", true, "digits are fine after the first character"},
		{"9LIVES", false, "an identifier cannot start with a digit"},
		{"", false, "empty is not a name"},
		{"sk-live-9f3c", false, "the reported shape: a real bearer"},
		{"$ATTACH_TOKEN", false, "an unexpanded reference is still not a name"},
		{"has space", false, "whitespace"},
		// A name-shaped secret, and the limit of a syntactic check: a
		// GitHub PAT is [A-Za-z0-9_] throughout, so nothing here can tell
		// it from a variable name. Pinned as `true` so the gap is written
		// down rather than rediscovered. The unset-variable warning is
		// what actually catches this one.
		{"ghp_aBcD1234", true, "indistinguishable from an env-var name by shape alone"},
	}
	for _, tc := range cases {
		if got := looksLikeEnvName(tc.in); got != tc.want {
			t.Errorf("looksLikeEnvName(%q) = %v, want %v (%s)", tc.in, got, tc.want, tc.why)
		}
	}
}
