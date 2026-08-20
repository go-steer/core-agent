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

// A typo'd TTL has to stop the run. Falling back to 5m would honour a
// budget the operator did not ask for, and the only place the downgrade
// would surface is the invoice.
func TestValidatePromptCacheTTL(t *testing.T) {
	t.Parallel()
	for _, ok := range []string{"", "5m", "1h"} {
		if err := validatePromptCacheTTL(ok); err != nil {
			t.Errorf("%q: got %v, want nil", ok, err)
		}
	}
	for _, bad := range []string{"1hr", "3600s", "60m", "1H", " 1h", "forever"} {
		err := validatePromptCacheTTL(bad)
		if err == nil {
			t.Errorf("%q: got nil, want an error", bad)
			continue
		}
		if !strings.Contains(err.Error(), "--prompt-cache-ttl") {
			t.Errorf("%q: error %q does not name the flag the operator typed", bad, err)
		}
	}
}
