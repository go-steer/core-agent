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

package childenv

import (
	"os"
	"slices"
	"testing"
)

// The set is process-global and has no reset, so every test uses names
// of its own and asserts only about those.

func TestFilterDropsWithheldNamesOnlyAndMatchesWholeNames(t *testing.T) {
	t.Parallel()
	Withhold("CE_TEST_TOKEN", "")
	got := filter([]string{
		"CE_TEST_TOKEN=secret",
		"CE_TEST_TOKEN_SUFFIX=keep", // a prefix match is not a match
		"PATH=/bin",
		"CE_TEST_EMPTY=",
	})
	want := []string{"CE_TEST_TOKEN_SUFFIX=keep", "PATH=/bin", "CE_TEST_EMPTY="}
	if !slices.Equal(got, want) {
		t.Fatalf("filter = %q, want %q", got, want)
	}
	if slices.Contains(Withheld(), "") {
		t.Fatal("an empty name was registered; optional config fields pass empty strings straight through")
	}
}

// A nil exec.Cmd.Env means "inherit everything", so Environ must never
// return nil — even for an empty parent environment.
func TestEnvironIsNeverNil(t *testing.T) {
	t.Parallel()
	if filter(nil) == nil {
		t.Fatal("filter(nil) returned nil; assigned to exec.Cmd.Env that inherits the full environment")
	}
}

// Not parallel: t.Setenv.
func TestHoldsCredentialNeedsAWithheldNameThatIsActuallySet(t *testing.T) {
	Withhold("CE_TEST_UNSET_TOKEN")
	t.Setenv("CE_TEST_UNSET_TOKEN", "")
	if HoldsCredential() && !anyOtherSet("CE_TEST_UNSET_TOKEN") {
		t.Fatal("an empty withheld variable counted as a held credential")
	}
	t.Setenv("CE_TEST_SET_TOKEN", "v")
	Withhold("CE_TEST_SET_TOKEN")
	if !HoldsCredential() {
		t.Fatal("a set, withheld variable did not count as a held credential")
	}
}

// anyOtherSet reports whether a withheld name other than skip is set by
// some other test in this process, which would make the negative half
// of the test above meaningless rather than wrong.
func anyOtherSet(skip string) bool {
	for _, n := range Withheld() {
		if n != skip {
			if v, ok := os.LookupEnv(n); ok && v != "" {
				return true
			}
		}
	}
	return false
}
