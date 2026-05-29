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

// Package permissions implements the central permission gate that
// decides whether each tool invocation may proceed.
//
// The gate consults, in order:
//  1. Bash denylist (built-in patterns; non-overridable) for bash calls.
//  2. Path scope check for file tools.
//  3. Config denylist patterns.
//  4. Config allowlist patterns.
//  5. Mode-specific resolution: ask → prompt user; allow → deny;
//     yolo → approve.
//
// The interactive prompt path is implemented by the host (TUI / CLI REPL);
// see prompter.go for the Prompter interface.
package permissions

import (
	"regexp"
	"strings"
)

// regexRule is a denylist entry: a regexp matched verbatim against the
// trimmed bash command, paired with a short user-facing reason.
type regexRule struct {
	pat    *regexp.Regexp
	reason string
}

var regexDenylist = []regexRule{
	{regexp.MustCompile(`\bdd\s+if=\S+\s+of=/dev/`), "refuses to write directly to a device file"},
	{regexp.MustCompile(`\bmkfs(\.[a-z0-9]+)?\b`), "refuses to format a filesystem"},
	{regexp.MustCompile(`\bshred\s+`), "refuses to securely overwrite files"},
	{regexp.MustCompile(`\bwipefs\s+`), "refuses to wipe filesystem signatures"},
	{regexp.MustCompile(`\bchmod\s+-R\s+[0-7]{3,4}\s+/(\s|$)`), "refuses to chmod the entire filesystem root"},
	{regexp.MustCompile(`\bchown\s+-R\s+\S+\s+/(\s|$)`), "refuses to chown the entire filesystem root"},
	{regexp.MustCompile(`\b(curl|wget)\s+\S[^|]*\|\s*(sh|bash|zsh|ash|dash)\b`), "refuses to execute a downloaded script directly"},
	{regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;\s*:`), "refuses to execute a fork bomb"},
}

// dangerousRmTargets lists path arguments that, combined with both -r
// and -f flags on rm, trigger a refusal. Compared after normalization.
var dangerousRmTargets = map[string]struct{}{
	"/":        {},
	"/*":       {},
	"~":        {},
	"~/":       {},
	"~/*":      {},
	"$HOME":    {},
	"$HOME/":   {},
	"${HOME}":  {},
	"${HOME}/": {},
	"/.":       {},
}

// IsBashDenied reports whether command matches any built-in denylist
// pattern. The reason is a short, user-facing string suitable for
// surfacing in a prompt or stderr.
func IsBashDenied(command string) (denied bool, reason string) {
	if r := checkDangerousRm(command); r != "" {
		return true, r
	}
	for _, r := range regexDenylist {
		if r.pat.MatchString(command) {
			return true, r.reason
		}
	}
	return false, ""
}

// checkDangerousRm returns a non-empty reason string if command is
// `rm`-with-recursive-and-force pointed at a destructive target. The
// flag parsing intentionally accepts any combination/order (-rf, -fr,
// -Rfv, --recursive --force, etc.).
func checkDangerousRm(command string) string {
	tokens := strings.Fields(strings.TrimSpace(command))
	if len(tokens) < 3 || tokens[0] != "rm" {
		return ""
	}
	hasR, hasF := false, false
	var positional []string
	for _, t := range tokens[1:] {
		switch {
		case t == "--recursive":
			hasR = true
		case t == "--force":
			hasF = true
		case strings.HasPrefix(t, "--"):
			// other long flags (e.g. --no-preserve-root) — ignored
		case strings.HasPrefix(t, "-"):
			for _, c := range t[1:] {
				switch c {
				case 'r', 'R':
					hasR = true
				case 'f', 'F':
					hasF = true
				}
			}
		default:
			positional = append(positional, t)
		}
	}
	if !hasR || !hasF {
		return ""
	}
	for _, p := range positional {
		if _, bad := dangerousRmTargets[p]; bad {
			return "refuses to recursively delete the filesystem root or $HOME"
		}
	}
	return ""
}
