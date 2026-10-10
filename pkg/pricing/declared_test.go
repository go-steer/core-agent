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

package pricing

import "testing"

// Declared rates price a model nothing else covers, and lose to every
// other layer for one something does: a profile's declaration is the
// operator's guess, a catalog row is a published price.
func TestDeclaredIsTheLowestLayer(t *testing.T) {
	var builtinID string
	for id := range builtin {
		builtinID = id
		break
	}
	c, err := NewCatalog(Options{Declared: map[string]ModelRates{
		"google/gemma-4-26B-A4B-it": {InputPerMTok: 0.1, OutputPerMTok: 0.4},
		builtinID:                   {InputPerMTok: 999, OutputPerMTok: 999},
	}})
	if err != nil {
		t.Fatal(err)
	}
	r, src, ok := c.LookupWithSource("google/gemma-4-26b-a4b-it")
	if !ok || src != SourceDeclared || r.InputPerMTok != 0.1 || r.OutputPerMTok != 0.4 {
		t.Errorf("declared-only model = %+v %q %v, want the declared rates from %q", r, src, ok, SourceDeclared)
	}
	if r, src, _ := c.LookupWithSource(builtinID); src != SourceBuiltin || r.InputPerMTok == 999 {
		t.Errorf("%s = %+v from %q, want the builtin row to outrank a declaration", builtinID, r, src)
	}
	if n := c.Counts().Declared; n != 2 {
		t.Errorf("Counts().Declared = %d, want 2", n)
	}
}
