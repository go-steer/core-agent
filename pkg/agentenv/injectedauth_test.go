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
