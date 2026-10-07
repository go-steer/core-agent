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
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"golang.org/x/term"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/runner"
)

// maxHashTokenInput bounds what `auth hash-token` reads from stdin. A
// bearer token is tens of bytes; anything near this is a wrong pipe.
const maxHashTokenInput = 4096

// shortTokenWarnLen is the length below which hash-token warns: a
// digest only hides a token that is too random to guess, and 32
// characters is half of what `openssl rand -hex 32` mints.
const shortTokenWarnLen = 32

const authUsage = `usage: core-agent auth hash-token < token-file

Reads one bearer token from stdin and prints the value for its users.json
row's "token_sha256" field. The token is never taken from the command
line, where it would land in shell history and the process list:

  printf '%s' "$TOKEN" | core-agent auth hash-token
  core-agent auth hash-token < /path/to/token

On a terminal it prompts without echoing.`

// runAuthSubcommand dispatches `core-agent auth ...` (#1213).
func runAuthSubcommand(args []string) int {
	isTTY := term.IsTerminal(int(os.Stdin.Fd()))
	readTTY := func() ([]byte, error) {
		fmt.Fprint(os.Stderr, "bearer token (not echoed): ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		return b, err
	}
	if !isTTY {
		readTTY = nil
	}
	return authSubcommand(args, os.Stdin, readTTY, os.Stdout, os.Stderr)
}

// authSubcommand is runAuthSubcommand with its streams injected. readTTY
// is non-nil when stdin is a terminal and reads one line without echo.
func authSubcommand(args []string, stdin io.Reader, readTTY func() ([]byte, error), stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "-h" || args[0] == "--help" || args[0] == "help" {
		_, _ = io.WriteString(stderr, authUsage+"\n")
		if len(args) == 0 {
			return runner.ExitConfigError
		}
		return runner.ExitOK
	}
	if args[0] != "hash-token" {
		fmt.Fprintf(stderr, "core-agent auth: unknown subcommand %q\n%s\n", args[0], authUsage)
		return runner.ExitConfigError
	}
	if len(args) > 1 {
		// Deliberately not echoed: the extra argument may be the token.
		fmt.Fprintf(stderr, "core-agent auth hash-token: takes no arguments; the token is read from stdin so it never appears in shell history or the process list\n%s\n", authUsage)
		return runner.ExitConfigError
	}

	var raw []byte
	var err error
	if readTTY != nil {
		raw, err = readTTY()
	} else {
		raw, err = io.ReadAll(io.LimitReader(stdin, maxHashTokenInput+1))
	}
	if err != nil {
		fmt.Fprintf(stderr, "core-agent auth hash-token: read stdin: %v\n", err)
		return runner.ExitConfigError
	}
	token, err := parseStdinToken(raw)
	if err != nil {
		fmt.Fprintf(stderr, "core-agent auth hash-token: %v\n", err)
		return runner.ExitConfigError
	}
	if len(token) < shortTokenWarnLen {
		fmt.Fprintf(stderr, "core-agent auth hash-token: warning: the token is %d characters; a short or guessable token can be recovered from its digest by trying candidates. Mint one with: openssl rand -hex 32\n", len(token))
	}
	fmt.Fprintln(stdout, auth.HashToken(token))
	return runner.ExitOK
}

// parseStdinToken extracts the one token from what stdin carried. A
// single trailing line ending is dropped (echo and a file's last line
// both add one); anything else that is not part of a token is refused
// rather than hashed, because a digest of the wrong string fails at
// request time with nothing to say why. No error quotes the input.
func parseStdinToken(raw []byte) (string, error) {
	if len(raw) > maxHashTokenInput {
		return "", fmt.Errorf("stdin is longer than %d bytes; expected one bearer token", maxHashTokenInput)
	}
	raw = bytes.TrimSuffix(raw, []byte("\n"))
	raw = bytes.TrimSuffix(raw, []byte("\r"))
	if len(raw) == 0 {
		return "", errors.New("stdin is empty; expected one bearer token")
	}
	s := string(raw)
	if strings.IndexFunc(s, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
		return "", errors.New("the token contains whitespace or a control character (more than one line, or padding?); a bearer token is one unbroken word, and its digest would match nothing a client sends")
	}
	return s, nil
}
