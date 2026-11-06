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

package permissions

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"

	"github.com/go-steer/core-agent/v3/pkg/config"
)

var (
	// ErrSandboxDenied is returned when the execution sandbox refuses a file
	// write, host-credential mutation, out-of-workspace mutation, or network
	// egress command.
	ErrSandboxDenied = errors.New("permissions: blocked by execution sandbox")

	// ErrSandboxUnavailable is returned when safety.sandbox.mode is "enforce"
	// and no OS or container sandbox driver can be resolved on the host.
	ErrSandboxUnavailable = errors.New("permissions: no execution sandbox driver available")
)

// Sandbox configures execution sandboxing and workspace write containment (#651).
type Sandbox struct {
	mode             string
	driver           string
	driverExplicit   bool
	network          string
	writablePaths    []string
	containerImage   string
	containerRuntime string

	lookPath   func(string) (string, error)
	probeBwrap func(string) bool
	goos       string
}

var (
	bwrapProbeOnce sync.Once
	bwrapProbeOK   bool
)

func defaultProbeBwrap(bin string) bool {
	bwrapProbeOnce.Do(func() {
		cmd := exec.Command(bin, "--ro-bind", "/", "/", "--dev", "/dev", "--proc", "/proc", "/bin/true") // #nosec G204
		bwrapProbeOK = cmd.Run() == nil
	})
	return bwrapProbeOK
}

// newSandbox constructs a Sandbox from config.SandboxConfig with normalized defaults.
func newSandbox(cfg config.SandboxConfig) *Sandbox {
	mode := strings.TrimSpace(cfg.Mode)
	if mode == "" {
		mode = config.SandboxModeAuto
	}
	driver := strings.TrimSpace(cfg.Driver)
	driverExplicit := driver != "" && driver != config.SandboxDriverAuto && driver != config.SandboxDriverPolicy
	if driver == "" {
		driver = config.SandboxDriverAuto
	}
	network := strings.TrimSpace(cfg.Network)
	if network == "" {
		network = config.SandboxNetworkAllow
	}
	writable := make([]string, 0, len(cfg.WritablePaths))
	for _, p := range cfg.WritablePaths {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		exp := expandUser(p)
		if abs, err := filepath.Abs(exp); err == nil {
			exp = filepath.Clean(abs)
		}
		writable = append(writable, exp)
		if resolved, rerr := resolveSymlinks(exp); rerr == nil && resolved != exp {
			writable = append(writable, resolved)
		}
	}
	return &Sandbox{
		mode:             mode,
		driver:           driver,
		driverExplicit:   driverExplicit,
		network:          network,
		writablePaths:    writable,
		containerImage:   strings.TrimSpace(cfg.ContainerImage),
		containerRuntime: strings.TrimSpace(cfg.ContainerRuntime),
		lookPath:         exec.LookPath,
		probeBwrap:       defaultProbeBwrap,
		goos:             runtime.GOOS,
	}
}

func (s *Sandbox) resolveContainerRuntime() (string, error) {
	if s.containerRuntime != "" {
		if _, err := s.lookPath(s.containerRuntime); err != nil {
			return "", fmt.Errorf("%w: container runtime %q not found on PATH: %v", ErrSandboxUnavailable, s.containerRuntime, err)
		}
		return s.containerRuntime, nil
	}
	for _, candidate := range []string{"docker", "podman"} {
		if _, err := s.lookPath(candidate); err == nil {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("%w: no container runtime (docker or podman) found on PATH", ErrSandboxUnavailable)
}

// Enabled reports whether sandboxing is active (mode != "off").
func (s *Sandbox) Enabled() bool {
	return s != nil && s.mode != config.SandboxModeOff
}

// Mode returns the resolved sandbox mode ("auto", "enforce", or "off").
func (s *Sandbox) Mode() string {
	if s == nil {
		return config.SandboxModeOff
	}
	return s.mode
}

// Driver returns the configured sandbox driver ("auto", "policy", "bwrap", "seatbelt", or "container").
func (s *Sandbox) Driver() string {
	if s == nil {
		return config.SandboxDriverAuto
	}
	return s.driver
}

// Network returns the configured sandbox network policy ("allow" or "none").
func (s *Sandbox) Network() string {
	if s == nil {
		return config.SandboxNetworkAllow
	}
	return s.network
}

// Sandbox returns the gate's active Sandbox configuration.
func (g *Gate) Sandbox() *Sandbox {
	if g == nil {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.sandbox
}

// SetSandbox replaces the gate's Sandbox configuration.
func (g *Gate) SetSandbox(cfg config.SandboxConfig) {
	if g == nil {
		return
	}
	sb := newSandbox(cfg)
	g.mu.Lock()
	g.sandbox = sb
	g.mu.Unlock()
}

// protectedHostHomeRelPaths are user-home relative paths whose modification
// compromises host credentials or shell startup persistence (#651).
var protectedHostHomeRelPaths = []string{
	".ssh",
	".gnupg",
	".aws",
	".kube",
	".docker",
	".netrc",
	".npmrc",
	".pypirc",
	".git-credentials",
	filepath.Join(".config", "gcloud"),
	filepath.Join(".config", "systemd"),
	".bashrc",
	".bash_profile",
	".bash_login",
	".profile",
	".zshrc",
	".zprofile",
	".zshenv",
	".zlogin",
}

// protectedHostCredentialDirs are user-home relative credential directories
// masked or denied for reads as well as writes inside OS sandboxes (#651).
var protectedHostCredentialDirs = []string{
	".ssh",
	".gnupg",
	".aws",
	".kube",
	filepath.Join(".config", "gcloud"),
}

// protectedHostSystemPrefixes are system paths whose modification compromises
// host persistence or privilege boundaries (#651).
var protectedHostSystemPrefixes = []string{
	"/etc/cron",
	"/etc/crontab",
	"/var/spool/cron",
	"/etc/systemd",
	"/etc/sudoers",
	"/etc/passwd",
	"/etc/shadow",
}

// isProtectedHostWritePath reports whether target resolves to a protected host
// credential or startup path.
func (g *Gate) isProtectedHostWritePath(target string) (string, bool) {
	candidates := candidatePaths(target)
	home, _ := os.UserHomeDir()
	var homeCandidates []string
	if home != "" {
		homeCandidates = candidatePaths(home)
	}
	for _, cand := range candidates {
		for _, sysPrefix := range protectedHostSystemPrefixes {
			if cand == sysPrefix || strings.HasPrefix(cand, sysPrefix+"/") || strings.HasPrefix(cand, sysPrefix+".") {
				if !g.isProjectRootStrictlyInside(sysPrefix) {
					return cand, true
				}
			}
		}
		for _, hc := range homeCandidates {
			for _, rel := range protectedHostHomeRelPaths {
				prot := filepath.Join(hc, rel)
				if cand == prot || isInside(cand, prot) {
					if !g.isProjectRootStrictlyInside(prot) {
						return cand, true
					}
				}
			}
		}
	}
	return "", false
}

// isProjectRootStrictlyInside reports whether any configured rwRoot is itself
// nested inside prot (e.g. a test whose fake projectRoot was created inside a
// sub-path). Unlike isInside(cand, rwRoot), launching core-agent with
// projectRoot == $HOME or "/" does NOT exempt ~/.ssh or /etc/crontab.
func (g *Gate) isProjectRootStrictlyInside(prot string) bool {
	if g == nil || g.scope == nil {
		return false
	}
	for _, root := range g.scope.rwRoots {
		if root != prot && isInside(root, prot) {
			return true
		}
	}
	return false
}

func candidatePaths(p string) []string {
	p = strings.TrimSpace(p)
	if p == "" {
		return nil
	}
	exp := expandUser(p)
	var out []string
	if abs, err := filepath.Abs(exp); err == nil {
		out = append(out, filepath.Clean(abs))
	} else {
		out = append(out, filepath.Clean(exp))
	}
	if resolved, err := ResolvePath(exp); err == nil && !slices.Contains(out, resolved) {
		out = append(out, resolved)
	}
	return out
}

func (g *Gate) hasWorkspaceBoundary(ctx context.Context) bool {
	if IsolatedWorktreeFromContext(ctx) != "" {
		return true
	}
	return g != nil && g.scope != nil && len(g.scope.rwRoots) > 0
}

// checkSandboxProtectedWrite blocks writes to protected host credential and
// startup files in all active sandbox modes ("auto" and "enforce").
func (g *Gate) checkSandboxProtectedWrite(toolName, path string) error {
	sb := g.Sandbox()
	if g == nil || !sb.Enabled() {
		return nil
	}
	if hit, blocked := g.isProtectedHostWritePath(path); blocked {
		return fmt.Errorf("%s refused (%w): write to protected host path %q is blocked while sandboxing is active", toolName, ErrSandboxDenied, hit)
	}
	return nil
}

// checkSandboxWorkspaceWrite enforces workspace write containment in ModeYolo
// and ModeAcceptEdits when a workspace root is configured and sandboxing is on.
func (g *Gate) checkSandboxWorkspaceWrite(ctx context.Context, toolName, path string) error {
	sb := g.Sandbox()
	if g == nil || !sb.Enabled() {
		return nil
	}
	if err := g.checkSandboxProtectedWrite(toolName, path); err != nil {
		return err
	}
	if !g.hasWorkspaceBoundary(ctx) && sb.mode != config.SandboxModeEnforce {
		return nil
	}
	if g.isSandboxWritableTarget(ctx, path) {
		return nil
	}
	return fmt.Errorf("%s refused (%w): path %q is outside the workspace sandbox boundary in %q mode; add it to path_scope.allow_paths / safety.sandbox.writable_paths or pass --no-sandbox",
		toolName, ErrSandboxDenied, path, g.Mode())
}

// isSandboxWritableTarget reports whether path's symlink-resolved canonical
// target is inside an allowed workspace, worktree, path_scope write entry,
// safety.sandbox.writable_paths entry, or system temp directory.
//
// Symlinks are resolved via ResolvePath before any allow check so a symlink
// inside /tmp, the workspace, or writable_paths pointing outside the boundary
// cannot bypass containment (#651). Unresolvable paths fail closed.
func (g *Gate) isSandboxWritableTarget(ctx context.Context, path string) bool {
	if _, blocked := g.isProtectedHostWritePath(path); blocked {
		return false
	}
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return false
	}
	if trimmed == "/dev/null" {
		return true
	}
	resolved, err := ResolvePath(expandUser(trimmed))
	if err != nil {
		return false
	}
	if resolved == "/dev/null" {
		return true
	}
	if _, blocked := g.isProtectedHostWritePath(resolved); blocked {
		return false
	}
	if wt := IsolatedWorktreeFromContext(ctx); wt != "" {
		if isInsideWorktree(resolved, wt) {
			return true
		}
	}
	if g.scope != nil {
		if acc, err := g.scope.AccessFor(resolved); err == nil && acc.Allows(AccessWrite) {
			return true
		}
	}
	if sb := g.Sandbox(); sb != nil {
		for _, wp := range sb.writablePaths {
			if isInside(resolved, wp) {
				return true
			}
		}
	}
	return g.isAllowedTempWrite(resolved)
}

// isAllowedTempWrite reports whether cand is inside /tmp, /var/tmp, or
// os.TempDir(), excluding sibling test tempdirs when the workspace root itself
// is a sub-directory of os.TempDir() (e.g., t.TempDir() in Go tests).
func (g *Gate) isAllowedTempWrite(cand string) bool {
	tempDirs := []string{"/tmp", "/var/tmp", "/private/tmp"}
	if td := filepath.Clean(os.TempDir()); td != "" && !slices.Contains(tempDirs, td) {
		tempDirs = append(tempDirs, td)
	}
	if resolvedTD, err := ResolvePath(os.TempDir()); err == nil && resolvedTD != "" && !slices.Contains(tempDirs, resolvedTD) {
		tempDirs = append(tempDirs, resolvedTD)
	}
	inTemp := false
	for _, td := range tempDirs {
		if isInside(cand, td) {
			inTemp = true
			break
		}
	}
	if !inTemp {
		return false
	}
	if g != nil && g.scope != nil {
		for _, rw := range g.scope.rwRoots {
			for _, td := range tempDirs {
				if isInside(rw, td) && rw != td {
					// Workspace root is inside tempDir (e.g. /tmp/TestFoo123/001).
					// Treat sibling subtrees under filepath.Dir(rw) (/tmp/TestFoo123/002)
					// as outside-workspace test fixtures rather than generic /tmp scratch.
					parent := filepath.Dir(rw)
					if parent != td && isInside(cand, parent) && !isInside(cand, rw) {
						return false
					}
				}
			}
		}
	}
	return true
}

// networkEgressBinaries are common CLI network tools refused in CheckBash when
// safety.sandbox.network is "none".
var networkEgressBinaries = map[string]struct{}{
	"curl":     {},
	"wget":     {},
	"nc":       {},
	"ncat":     {},
	"netcat":   {},
	"socat":    {},
	"ssh":      {},
	"scp":      {},
	"sftp":     {},
	"rsync":    {},
	"telnet":   {},
	"ftp":      {},
	"ping":     {},
	"dig":      {},
	"nslookup": {},
}

// checkSandboxBash enforces sandbox invariants on a shell command before it
// reaches policy/mode evaluation.
func (g *Gate) checkSandboxBash(ctx context.Context, command string) error {
	sb := g.Sandbox()
	if g == nil || !sb.Enabled() {
		return nil
	}
	mode := g.Mode()
	enforcedOrUnprompted := sb.mode == config.SandboxModeEnforce || mode == ModeYolo || mode == ModeAcceptEdits || g.prompter == nil
	if enforcedOrUnprompted {
		if target, blocked := g.findBashProtectedHostWrite(ctx, command); blocked {
			return fmt.Errorf("bash refused (%w): command modifies protected host path %q", ErrSandboxDenied, target)
		}
	}
	if sb.network == config.SandboxNetworkNone {
		if bin, hit := findBashNetworkBinary(command); hit {
			return fmt.Errorf("bash refused (%w): network command %q is blocked because safety.sandbox.network is %q", ErrSandboxDenied, bin, config.SandboxNetworkNone)
		}
	}
	if (mode == ModeYolo || mode == ModeAcceptEdits) && g.hasWorkspaceBoundary(ctx) {
		if outPath, hit := g.findBashOutOfWorkspaceWrite(ctx, command); hit {
			return fmt.Errorf("bash refused (%w): command writes to %q outside the workspace sandbox boundary in %q mode", ErrSandboxDenied, outPath, mode)
		}
	}
	if sb.mode == config.SandboxModeEnforce {
		if _, err := sb.resolveDriver(); err != nil {
			return err
		}
	}
	return nil
}

// findBashProtectedHostWrite scans a shell command's redirection targets and
// mutating verb arguments for protected host credential/startup paths.
func (g *Gate) findBashProtectedHostWrite(ctx context.Context, command string) (string, bool) {
	for _, target := range extractBashWriteTargets(command) {
		norm := normalizeShellPathToken(target)
		if norm == "" {
			continue
		}
		resolved := resolveAgainstWorkDir(ctx, norm)
		if hit, blocked := g.isProtectedHostWritePath(resolved); blocked {
			return hit, true
		}
	}
	return "", false
}

// findBashOutOfWorkspaceWrite scans a shell command's explicit write targets
// for paths outside the workspace boundary.
func (g *Gate) findBashOutOfWorkspaceWrite(ctx context.Context, command string) (string, bool) {
	for _, target := range extractBashWriteTargets(command) {
		norm := normalizeShellPathToken(target)
		if norm == "" {
			continue
		}
		resolved := resolveAgainstWorkDir(ctx, norm)
		if !g.isSandboxWritableTarget(ctx, resolved) {
			return norm, true
		}
	}
	return "", false
}

var bashMutatingVerbs = map[string]struct{}{
	"rm":       {},
	"mv":       {},
	"cp":       {},
	"touch":    {},
	"chmod":    {},
	"chown":    {},
	"truncate": {},
	"mkdir":    {},
	"rmdir":    {},
	"ln":       {},
	"install":  {},
	"tee":      {},
}

// extractBashWriteTargets returns file paths targeted by output redirections
// (`>`, `>>`, `>|`, `&>`) or common file-mutating verbs (`rm`, `mv`, `cp`,
// `touch`, `tee`, `chmod`, `chown`, `sed -i`, `dd of=...`).
func extractBashWriteTargets(command string) []string {
	var targets []string
	for _, seg := range splitShellSegments(command) {
		tokens := tokenizeShellWords(seg)
		if len(tokens) == 0 {
			continue
		}
		targets = append(targets, extractRedirectionTargets(tokens)...)
		verbIdx := firstCommandTokenIndex(tokens)
		if verbIdx < 0 || verbIdx >= len(tokens) {
			continue
		}
		verb := filepath.Base(tokens[verbIdx])
		args := stripRedirectionTokens(tokens[verbIdx+1:])
		if verb == "dd" {
			for _, a := range args {
				if strings.HasPrefix(a, "of=") {
					targets = append(targets, strings.TrimPrefix(a, "of="))
				}
			}
			continue
		}
		if verb == "sed" {
			TargetsFromSed(args, &targets)
			continue
		}
		if _, ok := bashMutatingVerbs[verb]; !ok {
			continue
		}
		targets = append(targets, positionalMutatingTargets(verb, args)...)
	}
	return targets
}

// TargetsFromSed appends in-place file targets when `sed -i` is used.
func TargetsFromSed(args []string, targets *[]string) {
	inPlace := false
	hasScriptFlag := false
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--in-place" || strings.HasPrefix(a, "--in-place=") {
			inPlace = true
			continue
		}
		if a == "-e" || a == "-f" || a == "--expression" || a == "--file" {
			hasScriptFlag = true
			i++ // skip script/file argument
			continue
		}
		if strings.HasPrefix(a, "--expression=") || strings.HasPrefix(a, "--file=") {
			hasScriptFlag = true
			continue
		}
		if strings.HasPrefix(a, "-") && !strings.HasPrefix(a, "--") && len(a) > 1 {
			flags := a[1:]
			if strings.ContainsRune(flags, 'i') {
				inPlace = true
			}
			if strings.ContainsRune(flags, 'e') || strings.ContainsRune(flags, 'f') {
				hasScriptFlag = true
				if strings.HasSuffix(flags, "e") || strings.HasSuffix(flags, "f") {
					i++ // next token is the script or script-file
				}
			}
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		pos = append(pos, a)
	}
	if !inPlace {
		return
	}
	if hasScriptFlag {
		*targets = append(*targets, pos...)
	} else if len(pos) >= 2 {
		*targets = append(*targets, pos[1:]...)
	}
}

func positionalMutatingTargets(verb string, args []string) []string {
	var pos []string
	var targetDir string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if (verb == "cp" || verb == "mv" || verb == "ln" || verb == "install") && (a == "-t" || a == "--target-directory") {
			if i+1 < len(args) {
				targetDir = args[i+1]
				i++
			}
			continue
		}
		if (verb == "cp" || verb == "mv" || verb == "ln" || verb == "install") && strings.HasPrefix(a, "--target-directory=") {
			targetDir = strings.TrimPrefix(a, "--target-directory=")
			continue
		}
		if (verb == "cp" || verb == "mv" || verb == "ln" || verb == "install") && strings.HasPrefix(a, "-t") && len(a) > 2 {
			targetDir = a[2:]
			continue
		}
		if strings.HasPrefix(a, "-") {
			continue
		}
		pos = append(pos, a)
	}
	if targetDir != "" {
		if verb == "mv" {
			return append([]string{targetDir}, pos...)
		}
		return []string{targetDir}
	}
	if len(pos) == 0 {
		return nil
	}
	switch verb {
	case "cp", "mv", "ln", "install":
		if verb == "mv" {
			return pos
		}
		return []string{pos[len(pos)-1]}
	case "chmod", "chown":
		if len(pos) >= 2 {
			return pos[1:]
		}
		return nil
	default:
		return pos
	}
}

func splitShellSegments(command string) []string {
	var segs []string
	var cur strings.Builder
	var quote byte
	for i := 0; i < len(command); i++ {
		c := command[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(command) {
				cur.WriteByte(c)
				i++
				cur.WriteByte(command[i])
				continue
			}
			cur.WriteByte(c)
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
			cur.WriteByte(c)
		case '|':
			if i > 0 && command[i-1] == '>' {
				// POSIX clobber redirection `>|`
				cur.WriteByte(c)
			} else {
				segs = append(segs, cur.String())
				cur.Reset()
			}
		case ';', '\n':
			segs = append(segs, cur.String())
			cur.Reset()
		case '&':
			if i+1 < len(command) && command[i+1] == '&' {
				segs = append(segs, cur.String())
				cur.Reset()
				i++
			} else if i > 0 && command[i-1] == '>' {
				cur.WriteByte(c)
			} else if i+1 < len(command) && command[i+1] == '>' {
				cur.WriteByte(c)
			} else {
				segs = append(segs, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteByte(c)
		}
	}
	if cur.Len() > 0 {
		segs = append(segs, cur.String())
	}
	return segs
}

func tokenizeShellWords(seg string) []string {
	var tokens []string
	var cur strings.Builder
	var quote byte
	flush := func() {
		if cur.Len() > 0 {
			tokens = append(tokens, cur.String())
			cur.Reset()
		}
	}
	for i := 0; i < len(seg); i++ {
		c := seg[i]
		if quote != 0 {
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && i+1 < len(seg) {
				i++
				cur.WriteByte(seg[i])
			} else {
				cur.WriteByte(c)
			}
			continue
		}
		switch c {
		case '\'', '"':
			quote = c
		case ' ', '\t', '\r', '(', ')':
			flush()
		case '>':
			flush()
			op := ">"
			if i+1 < len(seg) && (seg[i+1] == '>' || seg[i+1] == '|') {
				op = ">" + string(seg[i+1])
				i++
			}
			tokens = append(tokens, op)
		default:
			cur.WriteByte(c)
		}
	}
	flush()
	return tokens
}

func isRedirectionOperator(tok string) bool {
	switch tok {
	case ">", ">>", ">|", "1>", "1>>", "1>|", "2>", "2>>", "2>|", "&>", "&>>", "&>|":
		return true
	default:
		return false
	}
}

func extractRedirectionTargets(tokens []string) []string {
	var out []string
	for i := 0; i < len(tokens); i++ {
		if isRedirectionOperator(tokens[i]) {
			if i+1 < len(tokens) {
				target := tokens[i+1]
				if !strings.HasPrefix(target, "&") {
					out = append(out, target)
				}
				i++
			}
		}
	}
	return out
}

func stripRedirectionTokens(tokens []string) []string {
	var out []string
	for i := 0; i < len(tokens); i++ {
		if isRedirectionOperator(tokens[i]) {
			i++
			continue
		}
		out = append(out, tokens[i])
	}
	return out
}

func firstCommandTokenIndex(tokens []string) int {
	for i, tok := range tokens {
		if tok == "sudo" || tok == "env" || tok == "command" || tok == "nohup" {
			continue
		}
		if strings.Contains(tok, "=") && !strings.HasPrefix(tok, "-") && !strings.HasPrefix(tok, "/") {
			continue
		}
		return i
	}
	return -1
}

func normalizeShellPathToken(tok string) string {
	tok = strings.TrimSpace(tok)
	if tok == "" || strings.HasPrefix(tok, "-") || strings.HasPrefix(tok, "&") {
		return ""
	}
	for _, prefix := range []string{"$HOME/", "${HOME}/"} {
		if strings.HasPrefix(tok, prefix) {
			if home, err := os.UserHomeDir(); err == nil {
				return filepath.Join(home, strings.TrimPrefix(tok, prefix))
			}
		}
	}
	if tok == "$HOME" || tok == "${HOME}" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	return expandUser(tok)
}

func findBashNetworkBinary(command string) (string, bool) {
	for _, seg := range splitShellSegments(command) {
		tokens := tokenizeShellWords(seg)
		idx := firstCommandTokenIndex(tokens)
		if idx < 0 || idx >= len(tokens) {
			continue
		}
		base := filepath.Base(tokens[idx])
		if _, blocked := networkEgressBinaries[base]; blocked {
			return base, true
		}
	}
	return "", false
}

// resolveDriver selects and verifies the OS/container sandbox driver.
func (s *Sandbox) resolveDriver() (string, error) {
	if s == nil || s.mode == config.SandboxModeOff {
		return "", nil
	}
	switch s.driver {
	case config.SandboxDriverPolicy:
		if s.mode == config.SandboxModeEnforce {
			return "", fmt.Errorf("%w: driver=%q provides harness policy checks only and cannot satisfy mode=%q (want bwrap, seatbelt, or container)",
				ErrSandboxUnavailable, config.SandboxDriverPolicy, config.SandboxModeEnforce)
		}
		return config.SandboxDriverPolicy, nil

	case config.SandboxDriverBwrap:
		if s.goos != "linux" {
			return "", fmt.Errorf("%w: bwrap driver requires linux (running on %s)", ErrSandboxUnavailable, s.goos)
		}
		bin, err := s.lookPath("bwrap")
		if err != nil {
			return "", fmt.Errorf("%w: bwrap binary not found on PATH: %v", ErrSandboxUnavailable, err)
		}
		if !s.probeBwrap(bin) {
			return "", fmt.Errorf("%w: bwrap user/mount namespaces are unavailable on this host", ErrSandboxUnavailable)
		}
		return config.SandboxDriverBwrap, nil

	case config.SandboxDriverSeatbelt:
		if s.goos != "darwin" {
			return "", fmt.Errorf("%w: seatbelt driver requires darwin (running on %s)", ErrSandboxUnavailable, s.goos)
		}
		if _, err := s.lookPath("sandbox-exec"); err != nil {
			return "", fmt.Errorf("%w: sandbox-exec not found on PATH: %v", ErrSandboxUnavailable, err)
		}
		return config.SandboxDriverSeatbelt, nil

	case config.SandboxDriverContainer:
		if s.containerImage == "" {
			return "", fmt.Errorf("%w: container driver requires safety.sandbox.container_image", ErrSandboxUnavailable)
		}
		if _, err := s.resolveContainerRuntime(); err != nil {
			return "", err
		}
		return config.SandboxDriverContainer, nil

	case config.SandboxDriverAuto, "":
		if s.containerImage != "" {
			if _, err := s.resolveContainerRuntime(); err == nil {
				return config.SandboxDriverContainer, nil
			}
		}
		if s.goos == "linux" {
			if bin, err := s.lookPath("bwrap"); err == nil && s.probeBwrap(bin) {
				return config.SandboxDriverBwrap, nil
			}
		}
		if s.goos == "darwin" {
			if _, err := s.lookPath("sandbox-exec"); err == nil {
				return config.SandboxDriverSeatbelt, nil
			}
		}
		return "", fmt.Errorf("%w: no supported OS or container sandbox (bwrap, sandbox-exec, or container runtime) is available on %s", ErrSandboxUnavailable, s.goos)
	default:
		return "", fmt.Errorf("%w: unknown sandbox driver %q", ErrSandboxUnavailable, s.driver)
	}
}

// WrapBashCommand returns the executable and argument list for running a bash
// command under the gate's configured OS/container sandbox (#651).
//
// When mode is "enforce" or an explicit driver ("bwrap", "seatbelt",
// "container") is configured, the command is wrapped in the resolved OS or
// container sandbox. In "enforce" mode, an unavailable driver returns
// ErrSandboxUnavailable; in "auto" mode it falls back cleanly to "/bin/sh -c".
func (g *Gate) WrapBashCommand(ctx context.Context, command, workDir string) (string, []string, error) {
	g = g.resolveSessionGate(ctx)
	sb := g.Sandbox()
	if g == nil || !sb.Enabled() {
		return "/bin/sh", []string{"-c", command}, nil
	}
	if sb.mode != config.SandboxModeEnforce && !sb.driverExplicit {
		return "/bin/sh", []string{"-c", command}, nil
	}
	drv, err := sb.resolveDriver()
	if err != nil {
		if sb.mode == config.SandboxModeEnforce {
			return "", nil, err
		}
		return "/bin/sh", []string{"-c", command}, nil
	}
	writableDirs := g.sandboxWritableDirs(ctx, workDir)
	switch drv {
	case config.SandboxDriverBwrap:
		bin, _ := sb.lookPath("bwrap")
		if bin == "" {
			bin = "bwrap"
		}
		return bin, sb.buildBwrapArgs(command, workDir, writableDirs), nil
	case config.SandboxDriverSeatbelt:
		bin, _ := sb.lookPath("sandbox-exec")
		if bin == "" {
			bin = "sandbox-exec"
		}
		return bin, sb.buildSeatbeltArgs(command, writableDirs), nil
	case config.SandboxDriverContainer:
		rtName, _ := sb.resolveContainerRuntime()
		if rtName == "" {
			rtName = "docker"
		}
		bin, _ := sb.lookPath(rtName)
		if bin == "" {
			bin = rtName
		}
		return bin, sb.buildContainerArgs(command, workDir, writableDirs), nil
	default:
		return "/bin/sh", []string{"-c", command}, nil
	}
}

func (g *Gate) sandboxWritableDirs(ctx context.Context, workDir string) []string {
	var dirs []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" || strings.ContainsAny(p, "*?[") {
			return
		}
		p = filepath.Clean(expandUser(p))
		if !slices.Contains(dirs, p) {
			dirs = append(dirs, p)
		}
		if resolved, err := ResolvePath(p); err == nil && resolved != "" && !slices.Contains(dirs, resolved) {
			dirs = append(dirs, resolved)
		}
	}
	if wt := IsolatedWorktreeFromContext(ctx); wt != "" {
		add(wt)
	} else if g != nil && g.scope != nil {
		g.mu.Lock()
		rwRoots := slices.Clone(g.scope.rwRoots)
		g.mu.Unlock()
		for _, r := range rwRoots {
			add(r)
		}
	}
	if len(dirs) == 0 && workDir != "" {
		add(workDir)
	}
	if g != nil && g.scope != nil {
		g.mu.Lock()
		allowEntries := slices.Clone(g.scope.allow)
		g.mu.Unlock()
		for _, e := range allowEntries {
			if e.Access.Allows(AccessWrite) {
				add(strings.TrimSuffix(e.Pattern, "/..."))
			}
		}
	}
	if sb := g.Sandbox(); sb != nil {
		for _, wp := range sb.writablePaths {
			add(wp)
		}
	}
	add("/tmp")
	add("/private/tmp")
	if td := filepath.Clean(os.TempDir()); td != "" {
		add(td)
	}
	return dirs
}

func (s *Sandbox) buildBwrapArgs(command, workDir string, writableDirs []string) []string {
	args := []string{
		"--ro-bind", "/", "/",
		"--dev", "/dev",
		"--proc", "/proc",
		"--die-with-parent",
	}
	if s.network == config.SandboxNetworkNone {
		args = append(args, "--unshare-net")
	}
	for _, d := range writableDirs {
		if info, err := os.Stat(d); err == nil && (info.IsDir() || info.Mode().IsRegular()) {
			args = append(args, "--bind", d, d)
		}
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, rel := range protectedHostCredentialDirs {
			prot := filepath.Join(home, rel)
			if info, err := os.Stat(prot); err == nil && info.IsDir() {
				args = append(args, "--tmpfs", prot)
			}
		}
	}
	if workDir != "" {
		args = append(args, "--chdir", workDir)
	}
	args = append(args, "/bin/sh", "-c", command)
	return args
}

func escapeSeatbeltString(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	return strings.ReplaceAll(s, `"`, `\"`)
}

func (s *Sandbox) buildSeatbeltArgs(command string, writableDirs []string) []string {
	var prof strings.Builder
	prof.WriteString("(version 1)\n(allow default)\n(deny file-write*)\n")
	prof.WriteString("(allow file-write* (literal \"/dev/null\") (literal \"/dev/zero\") (literal \"/dev/tty\"))\n")
	for _, d := range writableDirs {
		escaped := escapeSeatbeltString(d)
		fmt.Fprintf(&prof, "(allow file-write* (subpath \"%s\"))\n", escaped)
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		for _, rel := range protectedHostCredentialDirs {
			prot := escapeSeatbeltString(filepath.Join(home, rel))
			fmt.Fprintf(&prof, "(deny file-read* (subpath \"%s\") (literal \"%s\"))\n", prot, prot)
		}
		for _, rel := range protectedHostHomeRelPaths {
			prot := escapeSeatbeltString(filepath.Join(home, rel))
			fmt.Fprintf(&prof, "(deny file-write* (subpath \"%s\") (literal \"%s\"))\n", prot, prot)
		}
	}
	if s.network == config.SandboxNetworkNone {
		prof.WriteString("(deny network*)\n")
	}
	return []string{"-p", prof.String(), "/bin/sh", "-c", command}
}

func (s *Sandbox) buildContainerArgs(command, workDir string, writableDirs []string) []string {
	args := []string{
		"run", "--rm", "-i",
		"--read-only",
		"--tmpfs", "/tmp:rw,noexec,nosuid",
	}
	if s.network == config.SandboxNetworkNone {
		args = append(args, "--network=none")
	}
	for _, d := range writableDirs {
		if d == "/tmp" || d == "/private/tmp" || strings.ContainsAny(d, ",\"") {
			continue
		}
		args = append(args, "--mount", fmt.Sprintf("type=bind,src=%s,dst=%s,rw", d, d))
	}
	if workDir != "" {
		args = append(args, "--workdir", workDir)
	} else if len(writableDirs) > 0 {
		args = append(args, "--workdir", writableDirs[0])
	}
	args = append(args, s.containerImage, "/bin/sh", "-c", command)
	return args
}
