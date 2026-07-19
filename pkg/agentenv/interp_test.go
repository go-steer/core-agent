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
	"reflect"
	"testing"
)

func TestInterpolateEnv(t *testing.T) {
	// t.Setenv is incompatible with t.Parallel — Go's testing package
	// enforces this to prevent env races. Since these subtests read the
	// process env, they run sequentially.
	t.Setenv("AGENTENV_FOO", "hello")
	t.Setenv("AGENTENV_BAR", "world")

	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"no refs", "just some text", "just some text"},
		{"single ref", "${env:AGENTENV_FOO}", "hello"},
		{"inline ref", "prefix ${env:AGENTENV_FOO} suffix", "prefix hello suffix"},
		{"multi refs", "${env:AGENTENV_FOO} ${env:AGENTENV_BAR}", "hello world"},
		{"repeated ref", "${env:AGENTENV_FOO} ${env:AGENTENV_FOO}", "hello hello"},
		{"unset var", "value=${env:AGENTENV_UNSET}", "value="},
		{"invalid syntax passes through", "${envAGENTENV_FOO} and $env:AGENTENV_FOO}", "${envAGENTENV_FOO} and $env:AGENTENV_FOO}"},
		{"digits in name", "${env:AGENTENV_FOO_123}", ""}, // unset, but syntactically valid
		{"leading-digit name is not matched", "${env:1BAD} ${env:AGENTENV_FOO}", "${env:1BAD} hello"},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			if got := InterpolateEnv(tc.in); got != tc.want {
				t.Errorf("InterpolateEnv(%q) = %q; want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestInterpolateMap(t *testing.T) {
	t.Setenv("AGENTENV_TOKEN", "sekret")

	tests := []struct {
		name string
		in   map[string]string
		want map[string]string
	}{
		{"nil", nil, nil},
		{"empty", map[string]string{}, nil},
		{
			"mixed",
			map[string]string{
				"Authorization": "Bearer ${env:AGENTENV_TOKEN}",
				"X-Static":      "value",
				"X-Unset":       "${env:AGENTENV_MISSING}",
			},
			map[string]string{
				"Authorization": "Bearer sekret",
				"X-Static":      "value",
				"X-Unset":       "",
			},
		},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			got := InterpolateMap(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("InterpolateMap = %#v; want %#v", got, tc.want)
			}
		})
	}
}

func TestFindReferences(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want []string
	}{
		{"none", "no interpolation here", nil},
		{"one", "hello ${env:FOO}", []string{"FOO"}},
		{"duplicates dedup", "${env:FOO} ${env:FOO} ${env:FOO}", []string{"FOO"}},
		{"sorted", "${env:ZZZ} ${env:AAA} ${env:MMM}", []string{"AAA", "MMM", "ZZZ"}},
		{"invalid ignored", "${env:1BAD} ${env:GOOD}", []string{"GOOD"}},
	}
	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := FindReferences(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("FindReferences(%q) = %v; want %v", tc.in, got, tc.want)
			}
		})
	}
}
