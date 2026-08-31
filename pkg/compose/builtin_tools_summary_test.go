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

package compose

import (
	"context"
	"testing"

	adkmodel "google.golang.org/adk/model"
)

// plainProvider has no server-side built-in concept — the echo /
// scripted shape.
type plainProvider struct{}

func (plainProvider) Name() string { return "plain" }
func (plainProvider) Model(context.Context, string) (adkmodel.LLM, error) {
	return nil, nil
}

// reportingProvider is the gemini / anthropic shape.
type reportingProvider struct {
	plainProvider
	names []string
}

func (p reportingProvider) BuiltinToolNames() []string { return p.names }

func TestBuiltinToolsSummary(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		in   interface {
			Name() string
			Model(context.Context, string) (adkmodel.LLM, error)
		}
		want string
	}{
		{
			name: "enabled built-ins are joined",
			in:   reportingProvider{names: []string{"web_search", "url_context"}},
			want: "web_search,url_context",
		},
		{
			// "none" and "" are different answers and the model line
			// renders them differently: a provider that HAS built-ins
			// and has them all off must say so, because the operator who
			// just wrote "web_search": false is reading this line to
			// confirm the key took. Silence would be indistinguishable
			// from a typo'd key — config decoding drops unknown fields
			// without complaint.
			name: "supported but all off reports none",
			in:   reportingProvider{},
			want: "none",
		},
		{
			name: "provider without the concept reports nothing",
			in:   plainProvider{},
			want: "",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := BuiltinToolsSummary(tc.in); got != tc.want {
				t.Errorf("BuiltinToolsSummary = %q, want %q", got, tc.want)
			}
		})
	}
}
