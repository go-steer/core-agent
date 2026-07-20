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

// Parsing helper for the --allow-path CLI flag. Split out of
// main.go so the syntax + edge cases can be tested without
// standing up a full flag.FlagSet.
//
// Syntax: PATH:ACCESS — explicit access is mandatory so a typo
// like `--allow-path /tmp` (with no spec) fails loudly instead of
// silently picking a permissive default. The path is split at the
// LAST `:` so pathological inputs like `/foo:bar:rw` resolve to
// `/foo:bar` with access `rw`.

package main

import (
	"fmt"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/config"
	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// parseAllowPathSpec parses one --allow-path argument into a typed
// config entry. The access portion is validated through
// permissions.ParseAccess so the accept set stays in lockstep
// with what FromConfig actually consumes.
//
// Errors are surfaced to flag.Parse via flag.Func's error return,
// which aborts startup with the message printed to stderr — the
// behavior we want for malformed flags.
func parseAllowPathSpec(spec string) (config.PathScopeAllowEntry, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return config.PathScopeAllowEntry{}, fmt.Errorf("--allow-path requires PATH:ACCESS (got empty)")
	}
	i := strings.LastIndex(spec, ":")
	if i < 0 {
		return config.PathScopeAllowEntry{}, fmt.Errorf("--allow-path requires explicit access (e.g. %q); got %q without colon", spec+":rw", spec)
	}
	path := strings.TrimSpace(spec[:i])
	access := strings.TrimSpace(spec[i+1:])
	if path == "" {
		return config.PathScopeAllowEntry{}, fmt.Errorf("--allow-path: path empty in %q (want PATH:ACCESS)", spec)
	}
	if access == "" {
		return config.PathScopeAllowEntry{}, fmt.Errorf("--allow-path: access empty in %q (want r, w, or rw)", spec)
	}
	if _, err := permissions.ParseAccess(access); err != nil {
		return config.PathScopeAllowEntry{}, fmt.Errorf("--allow-path %q: %w", spec, err)
	}
	return config.PathScopeAllowEntry{Path: path, Access: access}, nil
}
