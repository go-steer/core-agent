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

// Git, split across two repositories on purpose.
//
// The per-issue working copy lives on the workspace volume, where the
// agent's bash can write anything — including that copy's .git/config
// and hooks. Any git command the dispatcher ran in it afterwards would
// execute whatever the agent planted there (a hook, a core.fsmonitor
// command, a filter driver) in the dispatcher's pod, next to the App key
// and with the push token in reach.
//
// So the dispatcher keeps its own bare repository on a volume the agent
// cannot see (the private repo). Credentials only ever touch that one:
// it fetches the mirror's main there, the working copy is cloned FROM it
// (shallow, decision 20), and afterwards the agent's branch is fetched
// BACK into it, verified there, and pushed from there by SHA. The only
// git that runs inside the agent-writable copy after the agent has had it
// is the upload-pack serving that one fetch.
//
// Every invocation also pins the config an attacker would reach for:
// hooks off, fsmonitor off, the ext:: transport off, and the inherited
// credential helpers cleared.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// identity is the soak's commit identity (decision 16).
type identity struct {
	Name  string
	Email string
}

func (id identity) String() string { return id.Name + " <" + id.Email + ">" }

type gitOps struct {
	bin          string // git binary
	remote       string // the mirror's clone URL; never carries a credential
	base         string // the mirror branch work starts from ("main")
	privateDir   string // the dispatcher-only bare repo
	worktreesDir string // parent of the per-issue working copies
	id           identity
	// inspect vets the agent-writable copy before the fetch-back;
	// inspectCopy in production. A field so a test can prove the next
	// layer (runQuiet) holds on its own.
	inspect func(dir, base string) error
}

// hardening is prepended to every git invocation. `credential.helper=`
// (empty) resets the helper list, so a helper configured anywhere else —
// including in a repo's own config — is never consulted.
var hardening = []string{
	"-c", "core.hooksPath=/dev/null",
	"-c", "core.fsmonitor=false",
	"-c", "protocol.ext.allow=never",
	"-c", "credential.helper=",
}

// run executes git in dir (or with no -C when dir is ""), with extra -c
// args (credentials) placed after the hardening so they win.
func (g *gitOps) run(ctx context.Context, dir string, extra []string, args ...string) (string, error) {
	argv := append([]string{}, hardening...)
	argv = append(argv, extra...)
	if dir != "" {
		argv = append(argv, "-C", dir)
	}
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, g.bin, argv...) // #nosec G204 G702 -- fixed binary; args are built here, never from issue text.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", args[0], err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

// runQuiet is run for a command whose output must never be shown: every
// git process that reads the agent-writable copy, and every command over
// what it fetched. Git echoes the content of files it fails to parse —
// objects/info/alternates, packed-refs, info/grafts, an included config —
// and the agent can point any of those at a file in the dispatcher's pod,
// the App key included. The error carries the exit status and nothing
// else.
func (g *gitOps) runQuiet(ctx context.Context, dir string, extra []string, args ...string) error {
	argv := append([]string{}, hardening...)
	argv = append(argv, extra...)
	if dir != "" {
		argv = append(argv, "-C", dir)
	}
	argv = append(argv, args...)
	cmd := exec.CommandContext(ctx, g.bin, argv...) // #nosec G204 G702 -- fixed binary; args are built here, never from issue text.
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "LC_ALL=C")
	cmd.Stdout, cmd.Stderr = io.Discard, io.Discard
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("git %s exited with status %d", args[0], ee.ExitCode())
		}
		return fmt.Errorf("git %s did not run", args[0])
	}
	return nil
}

// credentialArgs returns the -c args that make git authenticate to an
// https remote with token, plus a cleanup that deletes the credential.
//
// The token goes into a 0600 file in a fresh 0700 directory under the
// dispatcher's own TMPDIR and reaches git through the stock `store`
// helper — never through argv (visible in ps) or the environment
// (visible in /proc/<pid>/environ and inherited by every child). The
// returned args name only the file. A non-https remote (a local path in
// tests) needs no credential and gets none.
func credentialArgs(remote, token string) ([]string, func(), error) {
	noop := func() {}
	u, err := url.Parse(remote)
	if err != nil || u.Scheme != "https" {
		return nil, noop, nil // not an https remote (a local path in tests): nothing to authenticate.
	}
	if token == "" {
		return nil, noop, errors.New("no token for an https remote")
	}
	dir, err := os.MkdirTemp("", "soak-dispatcher-cred-")
	if err != nil {
		return nil, noop, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	path := filepath.Join(dir, "credentials")
	// git runs a helper string containing spaces through the shell, so
	// the path is held to characters that need no quoting.
	if !safePathRe.MatchString(path) {
		cleanup()
		return nil, noop, fmt.Errorf("temp dir %q has characters git's helper string would need quoted; set TMPDIR to a plain path", dir)
	}
	line := (&url.URL{Scheme: "https", User: url.UserPassword("x-access-token", token), Host: u.Host}).String() + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		cleanup()
		return nil, noop, err
	}
	return []string{"-c", "credential.helper=store --file=" + path}, cleanup, nil
}

var safePathRe = regexp.MustCompile(`^[A-Za-z0-9/._-]+$`)

// ensurePrivate creates the private bare repo on first use.
func (g *gitOps) ensurePrivate(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(g.privateDir, "HEAD")); err == nil { // #nosec G703 -- operator-configured path.
		return nil
	}
	if err := os.MkdirAll(g.privateDir, 0o700); err != nil { // #nosec G703 -- operator-configured path.
		return err
	}
	_, err := g.run(ctx, "", nil, "init", "--bare", "--quiet", g.privateDir)
	return err
}

// fetchBase fetches the tip of the mirror's base branch into the private
// repo, depth 1, and returns its SHA.
func (g *gitOps) fetchBase(ctx context.Context, token string) (string, error) {
	if err := g.ensurePrivate(ctx); err != nil {
		return "", err
	}
	cred, cleanup, err := credentialArgs(g.remote, token)
	if err != nil {
		return "", err
	}
	defer cleanup()
	ref := "refs/heads/" + g.base
	if _, err := g.run(ctx, g.privateDir, cred, "fetch", "--quiet", "--no-tags", "--depth=1", g.remote, "+"+ref+":"+ref); err != nil {
		return "", err
	}
	return g.run(ctx, g.privateDir, nil, "rev-parse", "--verify", ref+"^{commit}")
}

func (g *gitOps) cloneDir(number int) string {
	return filepath.Join(g.worktreesDir, fmt.Sprintf("issue-%d", number))
}

// prepareClone makes the issue's working copy: a depth-1 clone of the
// base tip from the private repo, with no remote left behind, the issue
// branch checked out, and the soak identity configured locally.
//
// Depth 1 is decision 20: nothing that was ever in the mirror's history
// (the seed list with its expected outcomes) exists in the copy.
func (g *gitOps) prepareClone(ctx context.Context, number int) (string, error) {
	dir := g.cloneDir(number)
	if err := os.RemoveAll(dir); err != nil { // #nosec G703 -- under the operator's --worktrees-dir; number is an int.
		return "", fmt.Errorf("clear stale working copy: %w", err)
	}
	if err := os.MkdirAll(g.worktreesDir, 0o750); err != nil { // #nosec G703 -- operator-configured path.
		return "", err
	}
	src := (&url.URL{Scheme: "file", Path: g.privateDir}).String()
	if _, err := g.run(ctx, "", nil, "clone", "--quiet", "--depth=1", "--no-tags", "--single-branch", "--branch", g.base, src, dir); err != nil {
		return "", err
	}
	steps := [][]string{
		{"remote", "remove", "origin"},
		{"checkout", "--quiet", "-b", branchFor(number)},
		{"config", "user.name", g.id.Name},
		{"config", "user.email", g.id.Email},
	}
	for _, s := range steps {
		if _, err := g.run(ctx, dir, nil, s...); err != nil {
			return "", err
		}
	}
	return dir, nil
}

// commit is one commit on the agent's branch, as identity checks see it.
type commit struct {
	SHA            string
	AuthorName     string
	AuthorEmail    string
	CommitterName  string
	CommitterEmail string
	Message        string
}

// collect fetches the issue branch out of the working copy into the
// private repo and returns its tip and the commits past base. The fetch
// is the one git process that touches the agent-writable copy after the
// agent had it; safe.directory is scoped to that copy because the daemon
// and dispatcher may run as different users.
//
// Every git error on this path is reported by exit status only, never with
// git's output: see runQuiet.
func (g *gitOps) collect(ctx context.Context, number int, dir, base string) (string, []commit, error) {
	if err := g.inspect(dir, base); err != nil {
		return "", nil, fmt.Errorf("refusing to read the working copy: %w", err)
	}
	branch := branchFor(number)
	local := fmt.Sprintf("refs/soak/issue-%d", number)
	// A ref left by an earlier attempt must not survive a fetch that
	// quietly declined to update it (a shallow-root rejection warns and
	// exits 0), or the stale ref would be verified and pushed.
	if err := g.runQuiet(ctx, g.privateDir, nil, "update-ref", "-d", local); err != nil {
		return "", nil, fmt.Errorf("clear %s: %w", local, err)
	}
	src := (&url.URL{Scheme: "file", Path: dir}).String()
	safe := []string{"-c", "safe.directory=" + dir, "-c", "submodule.recurse=false", "-c", "fetch.recurseSubmodules=false"}
	if err := g.runQuiet(ctx, g.privateDir, safe, "fetch", "--quiet", "--no-tags", "--no-recurse-submodules", src, "+refs/heads/"+branch+":"+local); err != nil {
		return "", nil, fmt.Errorf("could not fetch %s from the working copy (%w; git's output is withheld because the copy is agent-controlled)", branch, err)
	}
	tip, err := g.run(ctx, g.privateDir, nil, "rev-parse", "--verify", "--quiet", local+"^{commit}")
	if err != nil {
		return "", nil, fmt.Errorf("the fetch did not produce %s (git's output is withheld)", branch)
	}
	if err := g.runQuiet(ctx, g.privateDir, nil, "merge-base", "--is-ancestor", base, tip); err != nil {
		return "", nil, fmt.Errorf("%s does not descend from the base %s", branch, base)
	}
	list, err := g.run(ctx, g.privateDir, nil, "rev-list", base+".."+tip)
	if err != nil {
		return "", nil, errors.New("could not list the branch's commits (git's output is withheld)")
	}
	var commits []commit
	for _, sha := range strings.Fields(list) {
		raw, err := g.run(ctx, g.privateDir, nil, "cat-file", "commit", sha)
		if err != nil {
			return "", nil, fmt.Errorf("could not read commit %.12s (git's output is withheld)", sha)
		}
		c, err := parseCommitObject(sha, raw)
		if err != nil {
			return "", nil, err
		}
		commits = append(commits, c)
	}
	return tip, commits, nil
}

// parseCommitObject reads identities and message straight out of the raw
// commit object (`git cat-file commit`), not out of a `git log --format`
// rendering. A format string needs separator bytes, and git accepts any
// byte in a name or a message, so an agent could forge field boundaries:
// a name carrying "\x1fsoak@…", or a message carrying the record
// separator, shifted fields and hid a trailer from the check. The object
// has one header per line, then a blank line, then the message, and no
// header value can contain a newline. A control byte in an identity is
// refused outright.
func parseCommitObject(sha, raw string) (commit, error) {
	head, msg, _ := strings.Cut(raw, "\n\n")
	c := commit{SHA: sha, Message: msg}
	seen := map[string]bool{}
	for _, line := range strings.Split(head, "\n") {
		key, val, _ := strings.Cut(line, " ")
		if key != "author" && key != "committer" {
			continue
		}
		if seen[key] {
			return commit{}, fmt.Errorf("commit %.12s has two %s headers", sha, key)
		}
		seen[key] = true
		name, email, err := parseIdentityHeader(val)
		if err != nil {
			return commit{}, fmt.Errorf("commit %.12s %s header: %w", sha, key, err)
		}
		if key == "author" {
			c.AuthorName, c.AuthorEmail = name, email
		} else {
			c.CommitterName, c.CommitterEmail = name, email
		}
	}
	if !seen["author"] || !seen["committer"] {
		return commit{}, fmt.Errorf("commit %.12s lacks an author or committer header", sha)
	}
	return c, nil
}

// parseIdentityHeader splits "Name <email> 1700000000 +0000".
func parseIdentityHeader(val string) (string, string, error) {
	open, closing := strings.Index(val, "<"), strings.Index(val, ">")
	if open < 0 || closing < open {
		return "", "", fmt.Errorf("malformed identity %q", val)
	}
	name, email := strings.TrimSpace(val[:open]), val[open+1:closing]
	if hasControl(name) || hasControl(email) || strings.ContainsAny(val[closing+1:], "<>") {
		return "", "", fmt.Errorf("identity %q has a control byte or a stray angle bracket", val)
	}
	return name, email, nil
}

func hasControl(s string) bool {
	return strings.IndexFunc(s, func(r rune) bool { return r < 0x20 || r == 0x7f }) >= 0
}

// push publishes tip — the exact SHA that was verified, not the branch
// name, so a commit the agent adds after verification is never pushed —
// as the issue branch on the mirror. Not forced: a branch left from an
// earlier attempt is a reason to stop, not to overwrite.
func (g *gitOps) push(ctx context.Context, number int, tip, token string) error {
	cred, cleanup, err := credentialArgs(g.remote, token)
	if err != nil {
		return err
	}
	defer cleanup()
	_, err = g.run(ctx, g.privateDir, cred, "push", "--quiet", "--no-verify", g.remote, tip+":refs/heads/"+branchFor(number))
	return err
}

// removeClone deletes the issue's working copy. No git involved: the
// copy is agent-writable, so nothing in it is executed to remove it.
func (g *gitOps) removeClone(number int) error {
	return os.RemoveAll(g.cloneDir(number)) // #nosec G703 -- under the operator's --worktrees-dir; number is an int.
}
