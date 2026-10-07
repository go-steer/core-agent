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

package childenv

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"
)

// MaxSecretFileBytes caps how much TakeFile reads. A bearer token is
// tens of bytes; anything near this size is the wrong file (a PEM bundle,
// a users table, /dev/zero), and an unbounded read of a device or a pipe
// nobody closes would hang startup instead of failing it.
const MaxSecretFileBytes = 64 << 10

// insecureFileBits are the permission bits TakeFile refuses on a regular
// file: any access by "other", and write by the group. World access means
// another user on the host can read the token (or replace it); group
// write means someone else can substitute one. Group READ is allowed on
// purpose: a Kubernetes secret volume mounted with fsGroup and
// defaultMode 0440 is the standard way to hand a token to a non-root
// container, and refusing it would push operators back to env vars.
const insecureFileBits = 0o027

// TakeFile is Take for a secret held in a file rather than an env var
// (#1201). It reads the file once, validates it, and — when it holds a
// value — makes the process non-dumpable, because the value now lives in
// this process's memory, which a same-user process can otherwise read
// through /proc/<pid>/mem the same way it reads /proc/<pid>/environ.
//
// A token read from a file never sits in any process's environment: not
// this one's, not a child's, and not the shell that started this one. A
// FIFO or a bash process substitution (`--token-file <(pass show x)`)
// goes further: the value is consumed by the read and there is nothing
// left on disk for anyone to read afterwards. A regular file stays where
// it is — TakeFile never deletes it — so it is only out of the agent's
// reach if the agent cannot read that path.
//
// Rules, each of which turns a mistake into a startup error instead of a
// bare 401 later:
//
//   - A regular file with any "other" permission bit, or group write, is
//     refused (see insecureFileBits). Pipes, character devices and
//     sockets are not mode-checked: /dev/fd/N from a process substitution
//     is a pipe, and its mode says nothing about who can read it.
//   - Surrounding whitespace, including the trailing newline every editor
//     and `echo` adds, is trimmed. What remains must be non-empty and
//     contain no whitespace or control characters: a bearer token has
//     none (RFC 6750 b64token), and a file with several lines is a
//     different file.
//   - More than MaxSecretFileBytes is refused.
//
// err is a read or validation failure, and value is then empty. protectErr
// is Protect's, and value is returned alongside it: failing to harden is a
// reason to warn, not to refuse to authenticate — the same contract as
// Take. They are separate results so a caller cannot turn a hardening
// failure into an outage by checking one error.
func TakeFile(path string) (value string, protectErr, err error) {
	value, err = ReadSecretFile(path)
	if err != nil {
		return "", nil, err
	}
	return value, Protect(), nil
}

// ReadSecretFile is TakeFile without the Protect step, for callers that
// manage hardening themselves and for tests that must not make the shared
// test process non-dumpable.
func ReadSecretFile(path string) (string, error) {
	if path == "" {
		return "", errors.New("token file path is empty")
	}
	f, err := os.Open(path) // #nosec G304 G703 -- the operator names this file on purpose.
	if err != nil {
		return "", fmt.Errorf("token file: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	if st.IsDir() {
		return "", fmt.Errorf("token file %s is a directory", path)
	}
	if st.Mode().IsRegular() {
		if perm := st.Mode().Perm(); perm&insecureFileBits != 0 {
			return "", fmt.Errorf("token file %s is accessible to other users (mode %04o); "+
				"chmod 0600 it (0640/0440 is fine; for a Kubernetes secret volume set defaultMode: 0400 or 0440)", path, perm)
		}
	}
	raw, err := io.ReadAll(io.LimitReader(f, MaxSecretFileBytes+1))
	if err != nil {
		return "", fmt.Errorf("token file %s: %w", path, err)
	}
	if len(raw) > MaxSecretFileBytes {
		return "", fmt.Errorf("token file %s is larger than %d bytes, so it is not a token", path, MaxSecretFileBytes)
	}
	return parseSecret(path, string(raw))
}

// parseSecret applies the content rules. It never echoes the content:
// the error is about a file that holds, or nearly holds, the secret.
func parseSecret(path, raw string) (string, error) {
	tok := strings.TrimSpace(raw)
	if tok == "" {
		return "", fmt.Errorf("token file %s is empty", path)
	}
	if i := strings.IndexFunc(tok, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }); i >= 0 {
		return "", fmt.Errorf("token file %s must hold the token alone on one line; it contains whitespace or a control character at byte %d", path, i)
	}
	return tok, nil
}
