// Copyright 2026 The Cogo Authors.
// SPDX-License-Identifier: Apache-2.0

package permissions

import "strings"

// extractBashVerb returns the leading command verb from a bash command,
// or "" when the command has no usable verb. Used by the gate to drive
// the "Allow `<verb> *` · session" prompt option and the session-wide
// verb allowlist, and (indirectly via SessionVerb-keyed entries) by the
// /permissions recommender.
//
// Rules:
//   - Leading KEY=VAL env assignments are skipped (`CGO_ENABLED=0 go build`
//     yields "go").
//   - A verb containing a slash (`./script.sh`, `/usr/bin/env`) returns "".
//     Those are too specific to broaden to "<verb> *".
//   - A verb containing a quote returns "" — quoting suggests args, not
//     a verb the user wants to trust wholesale.
//   - Empty or whitespace-only commands return "".
//
// The function intentionally does NOT consult the bash denylist; denied
// commands are blocked elsewhere in the gate and never reach this code.
func extractBashVerb(command string) string {
	for _, tok := range strings.Fields(command) {
		if strings.ContainsAny(tok, "'\"") {
			return ""
		}
		// Env-assignment guard: `KEY=VAL` with a leading identifier
		// character on the LHS. Anything starting with `-`, `.`, `/`
		// is not an assignment even if it contains `=`.
		if i := strings.IndexByte(tok, '='); i > 0 && isAssignmentKey(tok[:i]) {
			continue
		}
		if strings.ContainsAny(tok, "/\\") {
			return ""
		}
		// A token starting with `-` is a flag, not a verb. Refuse to
		// broaden permissions to "<flag> *" — that pattern would never
		// be useful and `--foo=bar` would otherwise leak through the
		// assignment guard above (LHS contains `-`, so it isn't an env
		// key, but the `=` doesn't make the token a verb either).
		if strings.HasPrefix(tok, "-") {
			return ""
		}
		return tok
	}
	return ""
}

// isAssignmentKey reports whether s looks like the LHS of a shell
// `KEY=VAL` env assignment: a non-empty identifier of [A-Za-z_][A-Za-z0-9_]*.
func isAssignmentKey(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
			// always allowed
		case r >= 'A' && r <= 'Z', r >= 'a' && r <= 'z':
			// allowed anywhere
		case r >= '0' && r <= '9':
			if i == 0 {
				return false
			}
		default:
			return false
		}
	}
	return true
}
