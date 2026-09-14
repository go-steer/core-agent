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

// The bearer-token flag takes the NAME of an environment variable, not
// a token (#947).
//
// That indirection is deliberate and is the whole security property of
// the flag: a token passed on a command line is visible in `ps ax` to
// every user on the box, lands in shell history, and is copied into any
// process listing a crash reporter or a container runtime happens to
// capture. Naming an env var keeps the secret out of argv.
//
// The flag was called `--token`, which reads as an instruction to hand
// over the token, and the failure mode for that natural misreading was
// silent: the literal bearer gets looked up as an env-var name, finds
// nothing, an empty Authorization header goes out, and the operator
// sees a bare 401 with nothing connecting it to the flag they typed.
// Observed 2026-07-13 on the v2.6 GKE demo drive with
// `--token "${SRE_TOKEN}"`, which is the shape a careful person types.
//
// So the flag is `--token-env`, `--token` stays as a deprecated alias
// with unchanged semantics, and the two ways to get this wrong now say
// so out loud before the request goes out. Both diagnostics are
// warnings on stderr rather than hard errors, for the same reason: the
// daemon may legitimately run with no token at all (Unix-socket attach,
// "Posture B"), so an empty resolution is a supported configuration and
// cannot be made fatal without breaking it.

package attachclient

import (
	"fmt"
	"io"
	"os"
	"strings"
)

// looksLikeEnvName reports whether s has the shape of a shell
// identifier, which is the only thing this flag can usefully be handed.
//
// Deliberately permissive about case. Lowercase env vars are unusual but
// entirely legal, and treating `attach_token` as "that's a secret, not a
// name" would produce a confident, wrong diagnostic — the failure this
// function exists to prevent, aimed the other way.
func looksLikeEnvName(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r == '_':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// ResolveTokenEnv turns the --token-env / --token flag values into the
// bearer token to send, writing any diagnosis to warn.
//
// cmd is the command name used in messages ("core-agent attach",
// "core-agent-tui"). tokenEnv is --token-env; legacy is the deprecated
// --token. warn may be nil, in which case the diagnostics are dropped —
// callers with nowhere to print still get the right token back.
//
// Returns "" for "no token", which is a supported posture and not an
// error: attach over a Unix socket and a daemon started without
// --attach-token both run unauthenticated by design.
func ResolveTokenEnv(cmd, tokenEnv, legacy string, warn io.Writer) string {
	if warn == nil {
		warn = io.Discard
	}
	name := strings.TrimSpace(tokenEnv)
	switch {
	case name != "" && strings.TrimSpace(legacy) != "":
		// Both given. --token-env wins because it is the flag that still
		// exists; saying so is what stops an operator from spending the
		// incident wondering which one the binary read.
		fmt.Fprintf(warn, "%s: both --token-env and --token were given; using --token-env=%s and ignoring --token\n", cmd, name)
	case name == "":
		name = strings.TrimSpace(legacy)
		if name != "" {
			fmt.Fprintf(warn, "%s: --token is deprecated; use --token-env (same meaning: the NAME of an env var)\n", cmd)
		}
	}
	if name == "" {
		return ""
	}
	if !looksLikeEnvName(name) {
		// Do not echo the value. It is almost certainly the secret, and
		// printing it here would put it in exactly the terminal scrollback
		// and log aggregator the env-var indirection exists to keep it out
		// of — turning a 401 into a leak.
		fmt.Fprintf(warn, "%s: --token-env takes the NAME of an env var, not the token itself "+
			"(e.g. --token-env=ATTACH_TOKEN, not --token-env=\"$ATTACH_TOKEN\"); "+
			"the value given is not a valid env-var name, so no token will be sent\n", cmd)
		return ""
	}
	tok := os.Getenv(name)
	if tok == "" {
		// The name is well-formed and resolves to nothing. Left alone this
		// is the same bare 401 as above, one step further along, and it is
		// the likelier of the two in a container where the env var was
		// simply never plumbed into the pod spec.
		fmt.Fprintf(warn, "%s: --token-env=%s names an env var that is unset or empty; "+
			"no token will be sent and an authenticated daemon will answer 401\n", cmd, name)
	}
	return tok
}
