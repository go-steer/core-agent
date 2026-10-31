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

package agentenv

import (
	"context"
	"strings"
	"testing"
)

func TestInjectedGoogleAuth(t *testing.T) {
	t.Setenv(EnvGoogleAuth, "")
	if InjectedGoogleAuth() {
		t.Fatal("InjectedGoogleAuth() = true with empty env, want false")
	}

	t.Setenv(EnvGoogleAuth, "other")
	if InjectedGoogleAuth() {
		t.Fatal("InjectedGoogleAuth() = true with non-injected env, want false")
	}

	t.Setenv(EnvGoogleAuth, GoogleAuthInjected)
	if !InjectedGoogleAuth() {
		t.Fatal("InjectedGoogleAuth() = false with injected env, want true")
	}
}

func TestInjectedGoogleTokenSource(t *testing.T) {
	t.Parallel()
	ts := InjectedGoogleTokenSource()
	tok, err := ts.Token()
	if err != nil {
		t.Fatalf("Token(): %v", err)
	}
	if tok.AccessToken != InjectedGooglePlaceholder {
		t.Errorf("AccessToken = %q, want %q", tok.AccessToken, InjectedGooglePlaceholder)
	}

	got, err := InjectedGoogleStatic{}.Token(context.Background())
	if err != nil {
		t.Fatalf("InjectedGoogleStatic.Token(): %v", err)
	}
	if got != InjectedGooglePlaceholder {
		t.Errorf("InjectedGoogleStatic.Token() = %q, want %q", got, InjectedGooglePlaceholder)
	}
}

// A mistyped value must be refused by name, not fall through to ADC and
// fail with an error about credentials (#1419).
func TestValidateGoogleAuthEnv(t *testing.T) {
	t.Parallel()
	for _, v := range []string{"", GoogleAuthInjected} {
		if err := ValidateGoogleAuthEnvFrom(func(string) string { return v }); err != nil {
			t.Errorf("ValidateGoogleAuthEnvFrom(%q) = %v, want nil", v, err)
		}
	}
	for _, v := range []string{"Injected", "inject", "true", "1", " injected"} {
		err := ValidateGoogleAuthEnvFrom(func(string) string { return v })
		if err == nil {
			t.Errorf("ValidateGoogleAuthEnvFrom(%q) = nil, want an error naming the variable", v)
			continue
		}
		for _, want := range []string{EnvGoogleAuth, v, GoogleAuthInjected} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("ValidateGoogleAuthEnvFrom(%q) error %q does not mention %q", v, err, want)
			}
		}
	}
}
