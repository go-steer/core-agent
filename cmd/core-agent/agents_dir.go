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
	"fmt"
	"os"
	"path/filepath"
)

// agentsDirSource names how the agentsDir was decided, for the startup
// summary. The value alone is not enough to debug a wrong one: the
// three ways of arriving at it are fixed in three different places
// (argv, argv, and a directory walk), and knowing which one applied is
// the difference between "fix your flag" and "you are running from the
// wrong cwd".
type agentsDirSource int

const (
	agentsDirNone      agentsDirSource = iota // nothing found
	agentsDirDiscovery                        // walked up from cwd
	agentsDirFromCfg                          // filepath.Dir(-c)
	agentsDirExplicit                         // --agents-dir
)

// String renders the source for the startup summary's agentsDir line.
func (s agentsDirSource) String() string {
	switch s {
	case agentsDirExplicit:
		return "via --agents-dir"
	case agentsDirFromCfg:
		return "derived from filepath.Dir(-c)"
	case agentsDirDiscovery:
		return "via .agents/ discovery"
	default:
		return ""
	}
}

// resolveAgentsDir applies --agents-dir over whatever loadConfig
// derived (#945).
//
// Precedence: --agents-dir > filepath.Dir(-c) > .agents/ discovered
// from cwd. The derivation stays the default, so nothing moves for an
// operator who does not pass the flag.
//
// An explicit --agents-dir that is not a directory is fatal rather
// than a warning. Everything downstream of this value degrades
// silently when it points nowhere — MCP loads no servers, skills load
// none, the env manifest is absent, record_plan has nowhere to write
// — so a typo would produce a daemon that starts cleanly and knows
// nothing. That failure is precisely why this flag exists, and it
// would be perverse to reintroduce it in the fix.
func resolveAgentsDir(flagValue, cfgPath, derived string) (string, agentsDirSource, error) {
	if flagValue != "" {
		info, err := os.Stat(flagValue)
		switch {
		case err != nil:
			return "", agentsDirNone, fmt.Errorf("--agents-dir %s: %w", flagValue, err)
		case !info.IsDir():
			return "", agentsDirNone, fmt.Errorf("--agents-dir %s: not a directory", flagValue)
		}
		return flagValue, agentsDirExplicit, nil
	}
	switch {
	case derived == "":
		return "", agentsDirNone, nil
	case cfgPath != "":
		return derived, agentsDirFromCfg, nil
	default:
		return derived, agentsDirDiscovery, nil
	}
}

// splitTreeWarning reports the one combination that is confusing
// rather than wrong: --agents-dir given with no -c, while config
// discovery landed on a config.json somewhere else.
//
// The daemon then reads its settings from one tree and its skills,
// MCP servers and plans from another. That is legal and occasionally
// wanted, but it is never what someone expects by accident, and the
// symptom — settings that apply, content that does not — looks like
// the content failed to load rather than like it was never looked
// for. Returns "" when there is nothing surprising to say.
//
// With -c present the split is the operator saying exactly what they
// meant, which is the whole point of the flag, so it stays quiet.
func splitTreeWarning(source agentsDirSource, cfgPath, agentsDir, discovered string) string {
	if source != agentsDirExplicit || cfgPath != "" || discovered == "" {
		return ""
	}
	if sameDir(discovered, agentsDir) {
		return ""
	}
	return fmt.Sprintf(
		"core-agent: --agents-dir %s, but config came from %s — settings and content are being read from different trees; pass -c %s to use one",
		agentsDir, filepath.Join(discovered, "config.json"), filepath.Join(agentsDir, "config.json"))
}

// sameDir compares two directory paths after resolving them as far as
// the filesystem allows, so "." and an absolute cwd, or a path through
// a symlink, do not read as a split tree.
func sameDir(a, b string) bool {
	if a == b {
		return true
	}
	ra, err := filepath.EvalSymlinks(a)
	if err != nil {
		ra, _ = filepath.Abs(a)
	}
	rb, err := filepath.EvalSymlinks(b)
	if err != nil {
		rb, _ = filepath.Abs(b)
	}
	return ra == rb
}
