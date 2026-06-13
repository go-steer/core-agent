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
	"flag"
	"strings"
)

// permuteFlags reorders args so every defined flag (and its value, if
// non-bool) lands first, followed by the positional args. The Go
// standard flag package stops parsing at the first non-flag argument
// — which makes
//
//	core-agent-tui http://host:7777 --token T --new-session
//
// silently drop `--token T` and `--new-session` because they appear
// after the positional URL. That's a sharp edge for an
// operator-facing CLI; permuting upfront so fs.Parse never sees
// positionals interleaved with flags makes both orderings work
// identically.
//
// Algorithm: walk args once; for each token starting with `-`:
//   - `--` or `-` alone: stop permuting (POSIX convention). Everything
//     remaining is positional.
//   - `--name=value` or `-name=value`: a single self-contained flag
//     token; goes to flags.
//   - `--name` / `-name` with name in the FlagSet:
//   - if the flag is a bool, it's self-contained → flags.
//   - otherwise the next arg is its value → both go to flags.
//   - unknown flag: pass through to flags so fs.Parse surfaces the
//     usual "flag provided but not defined" error rather than us
//     silently re-classifying it.
//
// Tokens that don't start with `-` are positionals.
//
// Returns (flagArgs, positionals). fs.Parse(flagArgs); fs.Args() will
// be empty (the caller uses the returned positionals slice instead).
func permuteFlags(fs *flag.FlagSet, args []string) (flagArgs, positionals []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		// Bare "-" is conventionally a positional (often a stand-in
		// for stdin); "--" is the explicit end-of-flags marker. In
		// both cases everything from here on is positional.
		if a == "-" || a == "--" {
			positionals = append(positionals, args[i+1:]...)
			return flagArgs, positionals
		}
		if !strings.HasPrefix(a, "-") {
			positionals = append(positionals, a)
			continue
		}
		// Strip leading dashes (we accept both `-name` and `--name`)
		// to look the flag up by its registered name.
		name := strings.TrimLeft(a, "-")
		// `--name=value` is self-contained.
		if idx := strings.Index(name, "="); idx >= 0 {
			flagArgs = append(flagArgs, a)
			continue
		}
		f := fs.Lookup(name)
		if f == nil {
			// Unknown flag — pass through so fs.Parse can produce its
			// standard error. We don't try to consume a value either,
			// since we don't know if the unknown flag takes one.
			flagArgs = append(flagArgs, a)
			continue
		}
		flagArgs = append(flagArgs, a)
		if isBoolFlag(f) {
			continue
		}
		// Non-bool flag consumes the next arg as its value, IF one
		// exists. The flag package will produce its own "flag needs
		// an argument" error otherwise; we match its behavior by
		// passing only the flag token in that case.
		if i+1 < len(args) {
			i++
			flagArgs = append(flagArgs, args[i])
		}
	}
	return flagArgs, positionals
}

// isBoolFlag reports whether f is a boolFlag — i.e., a flag that does
// NOT consume a separate value token. Mirrors the unexported check
// the flag package does in its own parser.
func isBoolFlag(f *flag.Flag) bool {
	bf, ok := f.Value.(interface{ IsBoolFlag() bool })
	return ok && bf.IsBoolFlag()
}
