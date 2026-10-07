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
	"strings"
	"testing"

	"github.com/go-steer/core-agent/v2/pkg/auth"
	"github.com/go-steer/core-agent/v2/pkg/runner"
)

const hashCLIToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" //nolint:gosec // test fixture

func runHashToken(t *testing.T, args []string, stdin string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errOut strings.Builder
	code = authSubcommand(args, strings.NewReader(stdin), nil, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestAuthHashToken_ReadsStdin(t *testing.T) {
	t.Parallel()
	want := auth.HashToken(hashCLIToken) + "\n"
	// printf, echo, and a CRLF file all name the same token.
	for _, in := range []string{hashCLIToken, hashCLIToken + "\n", hashCLIToken + "\r\n"} {
		code, out, errOut := runHashToken(t, []string{"hash-token"}, in)
		if code != runner.ExitOK || out != want {
			t.Errorf("stdin %q: code=%d stdout=%q stderr=%q, want %q", in, code, out, errOut, want)
		}
		if errOut != "" {
			t.Errorf("stdin %q: unexpected stderr %q", in, errOut)
		}
	}
}

// The token must never be accepted from argv, and the refusal must not
// echo it: an argument there is probably the token itself.
func TestAuthHashToken_RefusesArgv(t *testing.T) {
	t.Parallel()
	code, out, errOut := runHashToken(t, []string{"hash-token", hashCLIToken}, hashCLIToken)
	if code != runner.ExitConfigError || out != "" {
		t.Fatalf("code=%d stdout=%q, want a config error and no digest", code, out)
	}
	if !strings.Contains(errOut, "takes no arguments") {
		t.Errorf("stderr = %q, want the no-arguments refusal", errOut)
	}
	if strings.Contains(errOut, hashCLIToken) {
		t.Errorf("refusal echoes the argument: %q", errOut)
	}
}

func TestAuthHashToken_RejectsMalformedInput(t *testing.T) {
	t.Parallel()
	cases := map[string]struct{ in, want string }{
		"empty":           {"", "stdin is empty"},
		"just a newline":  {"\n", "stdin is empty"},
		"two lines":       {hashCLIToken + "\n" + hashCLIToken + "\n", "whitespace or a control character"},
		"padded":          {" " + hashCLIToken, "whitespace or a control character"},
		"inner tab":       {"abc\tdef", "whitespace or a control character"},
		"extra blank":     {hashCLIToken + "\n\n", "whitespace or a control character"},
		"oversized input": {strings.Repeat("a", maxHashTokenInput+1), "longer than"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			code, out, errOut := runHashToken(t, []string{"hash-token"}, tc.in)
			if code != runner.ExitConfigError || out != "" {
				t.Fatalf("code=%d stdout=%q, want a config error and no digest", code, out)
			}
			if !strings.Contains(errOut, tc.want) {
				t.Errorf("stderr = %q, want %q", errOut, tc.want)
			}
			if strings.Contains(errOut, hashCLIToken) {
				t.Errorf("error echoes the input: %q", errOut)
			}
		})
	}
}

func TestAuthHashToken_WarnsOnShortToken(t *testing.T) {
	t.Parallel()
	code, out, errOut := runHashToken(t, []string{"hash-token"}, "tok-alice\n")
	if code != runner.ExitOK || out != auth.HashToken("tok-alice")+"\n" {
		t.Fatalf("code=%d stdout=%q", code, out)
	}
	if !strings.Contains(errOut, "warning: the token is 9 characters") || strings.Contains(errOut, "tok-alice") {
		t.Errorf("stderr = %q, want a length warning that does not quote the token", errOut)
	}
}

func TestAuthHashToken_UsesTerminalReader(t *testing.T) {
	t.Parallel()
	var out, errOut strings.Builder
	readTTY := func() ([]byte, error) { return []byte(hashCLIToken), nil }
	code := authSubcommand([]string{"hash-token"}, strings.NewReader("ignored"), readTTY, &out, &errOut)
	if code != runner.ExitOK || out.String() != auth.HashToken(hashCLIToken)+"\n" {
		t.Errorf("code=%d stdout=%q, want the terminal-read token's digest", code, out.String())
	}

	failing := func() ([]byte, error) { return nil, errors.New("boom") }
	out.Reset()
	if code := authSubcommand([]string{"hash-token"}, nil, failing, &out, &errOut); code != runner.ExitConfigError || out.Len() != 0 {
		t.Errorf("read failure: code=%d stdout=%q", code, out.String())
	}
}

func TestAuthSubcommand_Usage(t *testing.T) {
	t.Parallel()
	if code, _, errOut := runHashToken(t, nil, ""); code != runner.ExitConfigError || !strings.Contains(errOut, "usage: core-agent auth hash-token") {
		t.Errorf("no args: code=%d stderr=%q", code, errOut)
	}
	if code, _, _ := runHashToken(t, []string{"--help"}, ""); code != runner.ExitOK {
		t.Errorf("--help: code=%d, want 0", code)
	}
	if code, _, _ := runHashToken(t, []string{"hash-token", "--help"}, ""); code != runner.ExitOK {
		t.Errorf("hash-token --help: code=%d, want 0", code)
	}
	// `core-agent auth <token>` is an easy slip; the refusal must not
	// echo what it was given.
	code, _, errOut := runHashToken(t, []string{hashCLIToken}, "")
	if code != runner.ExitConfigError || !strings.Contains(errOut, "unknown subcommand") {
		t.Errorf("unknown: code=%d stderr=%q", code, errOut)
	}
	if strings.Contains(errOut, hashCLIToken) {
		t.Errorf("unknown-subcommand refusal echoes its argument: %q", errOut)
	}
}
