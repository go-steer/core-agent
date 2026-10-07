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
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/attach"
	"github.com/go-steer/core-agent/v2/pkg/childenv"
)

// The attach listener's credential policy beyond #376. The settled
// decisions are in docs/local-listener-auth-design.md.

// resolveAttachToken reads the daemon's attach bearer token exactly once,
// from --attach-token-file or the env var --attach-token names, so the
// listener and hub registration share one value and nothing re-reads the
// environment later (#1201).
//
// An unset or empty --attach-token variable resolves to "" with no error
// here: the listener block refuses it (an authenticated listener with an
// empty token is a misconfiguration), while hub registration alone has
// always treated it as "register without a token". A token FILE that
// yields nothing is always an error — nobody names a file to mean "no
// token".
//
// The env path keeps its #1157 semantics: the variable stays in the
// daemon's environment, withheld from every child by
// withholdDaemonCredentials, which also makes the daemon non-dumpable.
// The file path never touches the environment, but the token still lives
// in this process's memory, so TakeFile makes the daemon non-dumpable
// too — /proc/<pid>/mem is as open to a same-user child as environ is.
func resolveAttachToken(o attachOpts, stderr io.Writer) (string, error) {
	o.TokenFile = strings.TrimSpace(o.TokenFile)
	switch {
	case o.TokenFile != "" && o.TokenEnv != "":
		return "", errors.New("--attach-token and --attach-token-file (attach.token_env and attach.token_file) are mutually exclusive; give one source for the attach token")
	case o.TokenFile != "":
		if childenv.MayBlock(o.TokenFile) {
			fmt.Fprintf(stderr, "core-agent: reading the attach token from %s, a pipe or device: startup waits until a writer has written it and closed\n", o.TokenFile)
		}
		tok, protectErr, err := childenv.TakeFile(o.TokenFile)
		if err != nil {
			return "", fmt.Errorf("--attach-token-file: %w", err)
		}
		switch {
		case !childenv.Supported:
			fmt.Fprintln(stderr, "core-agent: warning: this platform cannot make the daemon non-dumpable, so a child process can still read the attach token from the daemon's memory")
		case protectErr != nil:
			// Same stance as withholdDaemonCredentials: hardening that
			// failed is reported, not fatal.
			fmt.Fprintf(stderr, "core-agent: warning: could not make the daemon non-dumpable (%v); a child process can still read the attach token from /proc\n", protectErr)
		}
		return tok, nil
	case o.TokenEnv != "":
		return os.Getenv(o.TokenEnv), nil
	default:
		return "", nil
	}
}

// checkLocalListenerAuth refuses an attach listener with no credential
// gate when the agent has a shell (#1201).
//
// #376 already refuses an unauthenticated NON-loopback bind, and lets a
// loopback one start with a warning, on the theory that loopback means
// "this machine's own user". That theory fails exactly when the agent
// has the bash tool: bash runs as the listener's own user, so
// `curl 127.0.0.1:7777/sessions/<id>/perms/respond` — or
// `curl --unix-socket` against a 0600 socket — approves the agent's own
// permission prompt with no credential to steal. Loopback and socket
// permissions stop other users, never that one.
//
// shell is whether this build registered the bash tool. Without it the
// agent has no direct way to open a connection and the #376 posture
// stands. allowUnauthLocal is --attach-allow-unauthenticated-local: the
// operator's explicit acceptance, which downgrades the refusal to a
// warning that names what the agent can now do.
//
// Called after attach.NewServer, so a non-loopback bind has already been
// refused by its own error and every listener that reaches here is
// loopback or a Unix socket.
func checkLocalListenerAuth(opts attach.Options, shell, allowUnauthLocal bool, stderr io.Writer) error {
	// ReadOnly 403s every write before any handler runs, whatever the
	// credentials, so a read-only listener cannot answer a prompt, add a
	// rule or reset a guardrail. What it still serves is the agent's own
	// transcript, which is not this threat.
	if opts.Authenticated() || !shell || opts.Auth.ReadOnly {
		return nil
	}
	where := opts.Addr
	if opts.UnixSocket != "" {
		where = "unix socket " + opts.UnixSocket
	}
	if allowUnauthLocal {
		fmt.Fprintf(stderr, "core-agent: warning: --attach-allow-unauthenticated-local: the attach listener on %s has no authentication "+
			"and the agent's bash tool runs as this user, so the agent can approve its own permission prompts, add allow rules "+
			"and reset guardrails over it\n", where)
		return nil
	}
	return fmt.Errorf("refusing to start the attach listener on %s without authentication: the agent's bash tool runs as "+
		"this same user, so it could approve its own permission prompts (POST /sessions/<id>/perms/respond), add standing "+
		"allow rules and reset guardrails over this listener with no credential at all; loopback and socket-file "+
		"permissions stop other users, not this one (#1201). Give it a credential the agent cannot read: "+
		"--attach-token=<ENVVAR> (withheld from the agent's bash), or --attach-token-file=<path> naming a pipe such as "+
		"<(...) or a file the agent's user cannot read (a regular file the agent can cat is no credential); or mTLS "+
		"(--attach-tls-cert/--attach-tls-key with --attach-client-ca) or enforced multi-session auth. Or make it "+
		"--attach-readonly, remove the bash tool (--disable-tools=bash), or pass --attach-allow-unauthenticated-local "+
		"to accept the risk", where)
}

// attachTokenIfUsed resolves the attach token only when something will
// present or check it: a listener, or hub registration. A config
// attach.token_file must not make every `core-agent -p` read it, fail on
// it, or block forever opening a FIFO nobody is writing to.
func attachTokenIfUsed(o attachOpts, stderr io.Writer) (string, error) {
	if o.Listen == "" && o.UnixSocket == "" && o.RegisterTo == "" {
		return "", nil
	}
	return resolveAttachToken(o, stderr)
}
