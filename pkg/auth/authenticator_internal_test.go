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

package auth

import (
	"fmt"
	"strings"
	"testing"
)

// TestNewBearerTokenAuth_HoldsNoPlaintext pins that a legacy plaintext
// row is reduced to its digest at construction: the authenticator the
// daemon keeps for its lifetime holds no token, whichever shape the
// table used (#1213).
func TestNewBearerTokenAuth_HoldsNoPlaintext(t *testing.T) {
	t.Parallel()
	const legacy = "tok_legacy_plaintext_row_value"
	b := NewBearerTokenAuth([]User{{Identity: "bob@example.com", Token: legacy}}, nil, nil)
	if dump := fmt.Sprintf("%#v", *b); strings.Contains(dump, legacy) {
		t.Errorf("authenticator state holds the plaintext token:\n%s", dump)
	}
	if len(b.rows) != 1 {
		t.Fatalf("rows = %d, want 1", len(b.rows))
	}
}
