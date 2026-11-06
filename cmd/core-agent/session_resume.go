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
	"bufio"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/go-steer/core-models/adkv2"
	"google.golang.org/adk/v2/session"

	"github.com/go-steer/core-agent/v3/pkg/agent"
	"github.com/go-steer/core-agent/v3/pkg/compose"
	"github.com/go-steer/core-agent/v3/pkg/config"
	"github.com/go-steer/core-agent/v3/pkg/eventlog"
	"github.com/go-steer/core-agent/v3/pkg/runner"
	"github.com/go-steer/core-agent/v3/pkg/usage"
)

const (
	// resumePickSentinel is substituted by normalizeResumeArgs when
	// --resume is passed without an explicit session ID argument (#1374).
	resumePickSentinel = "@pick"

	// cliSessionUserID is the ADK UserID used by single-user CLI runs.
	cliSessionUserID = "local"

	// defaultCLISessionID is ADK's default bootstrap session ID used when
	// --session-db is off or when running an attach-mode daemon without
	// explicit --continue / --resume.
	defaultCLISessionID = "default"
)

// sessionOpts bundles the CLI session persistence and resume flags (#1374).
type sessionOpts struct {
	sessionDB       bool
	sessionDBSet    bool
	sessionDBPath   string
	continueSession bool
	resumeSession   string
}

// projectSessionSummary describes one stored root session for a project (#1374).
type projectSessionSummary struct {
	IDUpdated time.Time
	ID        string
	Preview   string
}

// sanitizeProjectSlug normalizes a directory base name into a lowercase
// ASCII slug ([a-z0-9-], max 20 runes), falling back to "proj" (#1374).
func sanitizeProjectSlug(base string) string {
	var b strings.Builder
	lastDash := false
	for _, r := range strings.ToLower(strings.TrimSpace(base)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			lastDash = false
		default:
			if b.Len() > 0 && !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
		if b.Len() >= 20 {
			break
		}
	}
	slug := strings.Trim(b.String(), "-")
	if slug == "" {
		return "proj"
	}
	return slug
}

// projectSessionPrefix returns the deterministic "<project-slug>-<12hex>"
// prefix for sessions belonging to projectRoot (#1374). Canonicalizing
// symlinks and relative segments ensures the same checkout resolves to the
// same prefix regardless of how the shell entered it.
func projectSessionPrefix(projectRoot string) string {
	canon := filepath.Clean(projectRoot)
	if abs, err := filepath.Abs(canon); err == nil {
		canon = abs
	}
	if eval, err := filepath.EvalSymlinks(canon); err == nil {
		canon = eval
	}
	slug := sanitizeProjectSlug(filepath.Base(canon))
	sum := sha256.Sum256([]byte(canon))
	return fmt.Sprintf("%s-%x", slug, sum[:6])
}

// newProjectSessionID mints a fresh session ID scoped to projectRoot
// ("<project-slug>-<12hex>-<8hex>") so two projects sharing one
// ~/.core-agent/sessions.db never collide on a shared "default" row (#1374).
func newProjectSessionID(projectRoot string) string {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return fmt.Sprintf("%s-%08x", projectSessionPrefix(projectRoot), time.Now().UnixNano()&0xffffffff)
	}
	return fmt.Sprintf("%s-%x", projectSessionPrefix(projectRoot), suffix[:])
}

// flagTakesSeparateValue reports whether arg (a flag token with no "=")
// is a registered non-boolean flag on fs that consumes the next argv token
// as its value (#1374).
func flagTakesSeparateValue(fs *flag.FlagSet, arg string) bool {
	if fs == nil || !strings.HasPrefix(arg, "-") || strings.Contains(arg, "=") {
		return false
	}
	name := strings.TrimLeft(arg, "-")
	if name == "" {
		return false
	}
	f := fs.Lookup(name)
	if f == nil || f.Value == nil {
		return false
	}
	if bf, ok := f.Value.(interface{ IsBoolFlag() bool }); ok && bf.IsBoolFlag() {
		return false
	}
	return true
}

// normalizeResumeArgs rewrites a bare --resume / -resume (at the end of
// argv or followed immediately by another flag) to --resume=@pick so Go's
// standard flag parser accepts both `--resume` (interactive picker) and
// `--resume <id>` (direct session ID) (#1374). When fs is non-nil,
// value-taking flags (such as `-p "--resume"`) skip their value token so
// a flag argument equal to "--resume" is never rewritten.
func normalizeResumeArgs(args []string, fs ...*flag.FlagSet) []string {
	var flagSet *flag.FlagSet
	if len(fs) > 0 {
		flagSet = fs[0]
	}
	out := make([]string, len(args))
	copy(out, args)
	for i := 0; i < len(out); i++ {
		arg := out[i]
		if arg == "--" {
			break
		}
		if arg == "--resume" || arg == "-resume" {
			if i+1 >= len(out) || strings.HasPrefix(out[i+1], "-") {
				out[i] = "--resume=" + resumePickSentinel
			} else {
				i++ // skip explicit <session-id> value
			}
			continue
		}
		if arg == "--resume=" || arg == "-resume=" {
			out[i] = "--resume=" + resumePickSentinel
			continue
		}
		if flagTakesSeparateValue(flagSet, arg) {
			i++ // skip the value consumed by this flag
		}
	}
	return out
}

// resolveSessionDBWithResume extends resolveSessionDB with the #1374
// implication that --continue and --resume require a durable session DB.
func resolveSessionDBWithResume(sessionDB, sessionDBSet bool, sessionDBPath, attachListen, attachUnixSocket string, externalTranscript, resuming bool) (enabled, implied bool, impliedBy string, conflict bool) {
	attaching := attachListen != "" || attachUnixSocket != ""
	if attaching || externalTranscript {
		enabled, implied, conflict = resolveSessionDB(sessionDB, sessionDBSet, sessionDBPath, attachListen, attachUnixSocket, externalTranscript)
		if externalTranscript {
			return enabled, implied, "state.transcript.backend=", conflict
		}
		return enabled, implied, "attach mode", conflict
	}
	if resuming {
		if sessionDBSet && !sessionDB && sessionDBPath == "" {
			return false, false, "--continue/--resume", true
		}
		if sessionDB || sessionDBPath != "" {
			return sessionDB, false, "", false
		}
		return true, true, "--continue/--resume", false
	}
	enabled, implied, conflict = resolveSessionDB(sessionDB, sessionDBSet, sessionDBPath, attachListen, attachUnixSocket, false)
	return enabled, implied, "", conflict
}

// isDerivedSessionID reports whether sid is a subagent or internal derived
// session ID (such as "<root>:sub:...", "<root>:ops", or "<root>:digest")
// rather than a top-level root session (#1374).
func isDerivedSessionID(sid string) bool {
	return eventlog.RootOf(sid) != sid || strings.Contains(sid, ":")
}

// listProjectSessions returns root sessions in svc whose ID starts with
// projectSessionPrefix(projectRoot)+"-", ordered newest-first (#1374).
// When withPreviews is false (e.g. for --continue, which only needs the
// newest session ID), it skips per-session preview extraction so it never
// triggers O(N) svc.Get event loads.
func listProjectSessions(ctx context.Context, svc session.Service, projectRoot string, withPreviews bool) ([]projectSessionSummary, error) {
	if svc == nil {
		return nil, fmt.Errorf("session service is nil")
	}
	resp, err := svc.List(ctx, &session.ListRequest{
		AppName: agent.DefaultAppName,
		UserID:  cliSessionUserID,
	})
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	prefix := projectSessionPrefix(projectRoot) + "-"
	type candidate struct {
		sess    session.Session
		summary projectSessionSummary
	}
	var cands []candidate
	for _, s := range resp.Sessions {
		if s == nil {
			continue
		}
		sid := s.ID()
		if isDerivedSessionID(sid) {
			continue
		}
		if !strings.HasPrefix(sid, prefix) {
			continue
		}
		cands = append(cands, candidate{
			sess: s,
			summary: projectSessionSummary{
				ID:        sid,
				IDUpdated: s.LastUpdateTime(),
			},
		})
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].summary.IDUpdated.Equal(cands[j].summary.IDUpdated) {
			return cands[i].summary.IDUpdated.After(cands[j].summary.IDUpdated)
		}
		return cands[i].summary.ID > cands[j].summary.ID
	})
	out := make([]projectSessionSummary, len(cands))
	for i, c := range cands {
		if withPreviews {
			c.summary.Preview = extractSessionPreview(ctx, svc, c.sess)
		}
		out[i] = c.summary
	}
	return out, nil
}

// extractSessionPreview returns a short single-line label for a session
// from its state title or first user turn (#1374).
func extractSessionPreview(ctx context.Context, svc session.Service, s session.Session) string {
	if s == nil {
		return ""
	}
	if p := sessionPreviewFrom(s); p != "" {
		return p
	}
	if svc == nil {
		return ""
	}
	got, err := svc.Get(ctx, &session.GetRequest{
		AppName:   s.AppName(),
		UserID:    s.UserID(),
		SessionID: s.ID(),
	})
	if err != nil || got == nil || got.Session == nil {
		return ""
	}
	return sessionPreviewFrom(got.Session)
}

func sessionPreviewFrom(s session.Session) string {
	if s == nil {
		return ""
	}
	if st := s.State(); st != nil {
		if v, err := st.Get("title"); err == nil {
			if title, ok := v.(string); ok && strings.TrimSpace(title) != "" {
				return truncatePreview(title, 60)
			}
		}
	}
	if preview := firstUserText(s.Events()); preview != "" {
		return truncatePreview(preview, 60)
	}
	return ""
}

func firstUserText(events session.Events) string {
	if events == nil {
		return ""
	}
	for ev := range events.All() {
		if ev == nil || ev.Author != "user" || ev.Content == nil {
			continue
		}
		for _, p := range ev.Content.Parts {
			if p != nil && strings.TrimSpace(p.Text) != "" {
				return strings.TrimSpace(p.Text)
			}
		}
	}
	return ""
}

func truncatePreview(s string, maxRunes int) string {
	line := strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	runes := []rune(line)
	if len(runes) <= maxRunes {
		return line
	}
	return string(runes[:maxRunes-1]) + "…"
}

// resolveCLISessionID resolves the session ID for a CLI run (#1374).
// Returns (sessionID, resumed, error).
func resolveCLISessionID(
	ctx context.Context,
	svc session.Service,
	projectRoot string,
	continueFlag bool,
	resumeFlag string,
	in io.Reader,
	out io.Writer,
	isTTY bool,
) (string, bool, error) {
	if continueFlag && resumeFlag != "" {
		return "", false, fmt.Errorf("cannot combine --continue and --resume; pass --continue for the latest project session or --resume [id] to select one")
	}
	if !continueFlag && resumeFlag == "" {
		return newProjectSessionID(projectRoot), false, nil
	}
	if svc == nil {
		return "", false, fmt.Errorf("--continue and --resume require a durable session database (--session-db)")
	}
	if continueFlag {
		sessions, err := listProjectSessions(ctx, svc, projectRoot, false)
		if err != nil {
			return "", false, err
		}
		if len(sessions) == 0 {
			return "", false, fmt.Errorf("--continue: no previous session found for project %s", projectRoot)
		}
		return sessions[0].ID, true, nil
	}
	if resumeFlag != resumePickSentinel {
		target := strings.TrimSpace(resumeFlag)
		if target == "" {
			return "", false, fmt.Errorf("--resume: session ID cannot be empty")
		}
		if isDerivedSessionID(target) {
			return "", false, fmt.Errorf("--resume %q: cannot resume a derived subagent or internal session directly", target)
		}
		if _, err := svc.Get(ctx, &session.GetRequest{
			AppName:   agent.DefaultAppName,
			UserID:    cliSessionUserID,
			SessionID: target,
		}); err != nil {
			return "", false, fmt.Errorf("--resume %q: session not found in session db: %w", target, err)
		}
		return target, true, nil
	}
	sessions, err := listProjectSessions(ctx, svc, projectRoot, true)
	if err != nil {
		return "", false, err
	}
	if len(sessions) == 0 {
		return "", false, fmt.Errorf("--resume: no previous sessions found for project %s", projectRoot)
	}
	picked, err := pickSessionInteractively(projectRoot, sessions, in, out, isTTY)
	if err != nil {
		return "", false, err
	}
	return picked, true, nil
}

func pickSessionInteractively(projectRoot string, sessions []projectSessionSummary, in io.Reader, out io.Writer, isTTY bool) (string, error) {
	if !isTTY || in == nil {
		return "", fmt.Errorf("--resume without a session ID requires an interactive terminal; pass --resume <session-id> or --continue in non-interactive runs")
	}
	if out == nil {
		out = io.Discard
	}
	fmt.Fprintf(out, "Sessions for %s:\n", projectRoot)
	for i, s := range sessions {
		ts := "unknown time"
		if !s.IDUpdated.IsZero() {
			ts = s.IDUpdated.Local().Format("2006-01-02 15:04:05")
		}
		if s.Preview != "" {
			fmt.Fprintf(out, "  [%d] %s  (%s)  %q\n", i+1, s.ID, ts, s.Preview)
		} else {
			fmt.Fprintf(out, "  [%d] %s  (%s)\n", i+1, s.ID, ts)
		}
	}
	fmt.Fprintf(out, "Select session [1-%d] (Enter for 1): ", len(sessions))

	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil && err != io.EOF {
		return "", fmt.Errorf("--resume picker read: %w", err)
	}
	choice := strings.TrimSpace(line)
	if choice == "" {
		return sessions[0].ID, nil
	}
	if idx, convErr := strconv.Atoi(choice); convErr == nil {
		if idx < 1 || idx > len(sessions) {
			return "", fmt.Errorf("--resume picker: selection %d out of range [1-%d]", idx, len(sessions))
		}
		return sessions[idx-1].ID, nil
	}
	for _, s := range sessions {
		if s.ID == choice {
			return s.ID, nil
		}
	}
	return "", fmt.Errorf("--resume picker: unknown selection %q (want 1-%d or session ID)", choice, len(sessions))
}

// restoreAndWireCLISpend restores historical usage into tracker when a CLI
// session is resumed, and wires the durable SpendStore via pkg/compose so
// subsequent turns persist their billed cost (#1374, #1433).
func restoreAndWireCLISpend(
	ctx context.Context,
	handle *eventlog.Handle,
	cfg *config.Config,
	tracker *usage.Tracker,
	sessionID string,
	resumed bool,
	modelName string,
	pricingRate usage.Pricing,
	send func(string),
) {
	if handle == nil || tracker == nil {
		return
	}
	if resumed {
		restored := compose.RestoreResumedTracker(ctx, handle, cfg, cliSessionUserID, sessionID, tracker, modelName, pricingRate)
		if send != nil {
			send(fmt.Sprintf("session %s: resumed (%d prior turn(s) restored)", sessionID, restored))
		}
	}
	compose.InstallSpendLedger(ctx, handle.Spend, tracker, cliSessionUserID, sessionID)
}

// openEventlogIfEnabled resolves and opens the SQLite session DB when
// requested or implied by attach mode / external transcript / --continue / --resume (#1374).
func openEventlogIfEnabled(
	ctx context.Context,
	cfg *config.Config,
	sessCfg sessionOpts,
	attachCfg attachOpts,
	prompt, scriptPath string,
	stop func(),
	send func(string),
) (*eventlog.Handle, error) {
	resuming := sessCfg.continueSession || sessCfg.resumeSession != ""
	attaching := attachCfg.Listen != "" || attachCfg.UnixSocket != ""
	externalTranscript := cfg != nil && cfg.TranscriptIsExternal()
	sessionDB, impliedSessionDB, impliedBy, sessionDBConflict := resolveSessionDBWithResume(
		sessCfg.sessionDB, sessCfg.sessionDBSet,
		sessCfg.sessionDBPath, attachCfg.Listen, attachCfg.UnixSocket, externalTranscript, resuming)
	if impliedBy == "state.transcript.backend=" {
		impliedBy = sessionDBImplier(cfg, attaching)
	}
	if sessionDBConflict {
		if resuming && !attaching && !externalTranscript {
			return nil, fmt.Errorf("--session-db=false cannot be combined with --continue or --resume: resuming a session requires the session database\ncore-agent:   drop --session-db=false, or pass --session-db-path=PATH to choose where it lands")
		}
		return nil, fmt.Errorf("%s\ncore-agent:   drop --session-db=false, or pass --session-db-path=PATH to choose where it lands", sessionDBConflictMessage(cfg))
	}
	if !sessionDB && sessCfg.sessionDBPath == "" {
		if prompt == "" && scriptPath == "" {
			send("session db: off (in-memory) — no resume, no auto-continue, no guardrail state across restart; this session is gone when the process exits. --session-db to persist.")
		}
		return nil, nil
	}
	path, err := resolveSessionDBPath(sessCfg.sessionDBPath)
	if err != nil {
		return nil, fmt.Errorf("--session-db-path: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		if impliedSessionDB {
			if resuming && !attaching && !externalTranscript {
				return nil, fmt.Errorf("%s needs a durable session db, and %s is not writable: %w\ncore-agent:   pass --session-db-path=/some/writable/path/sessions.db", impliedBy, filepath.Dir(path), err)
			}
			return nil, fmt.Errorf("%s needs a local session db, and %s is not writable: %w\ncore-agent:   pass --session-db-path=/some/writable/path/sessions.db", sessionDBImplier(cfg, attaching), filepath.Dir(path), err)
		}
		return nil, fmt.Errorf("session db dir: %w", err)
	}
	h, storeLine, err := openSessionStore(ctx, cfg, path, stop)
	if err != nil {
		return nil, err
	}
	switch cfg.Model.Provider {
	case config.ProviderGemini, config.ProviderVertex:
		h.Service = adkv2.GroundingProjection(h.Service)
	}
	printSessionStoreLines(send, path, impliedSessionDB, impliedBy, storeLine)
	return h, nil
}

// openCLISessionAndEventlog resolves the session DB and per-project session ID
// (fresh, --continue, or --resume), opens the eventlog handle when enabled,
// restores tracker spend on resume, and appends WithEventLog + WithSession to opts (#1374).
//
// In-memory runs (handle == nil) and attach-mode daemons without explicit
// --continue / --resume preserve ADK's default bootstrap session ID ("default")
// so attach endpoints (/sessions/default/*), the bootstrap DELETE guard, and
// --no-repl single-session auto-continue keep their established contract.
func openCLISessionAndEventlog(
	ctx context.Context,
	cfg *config.Config,
	projectRoot string,
	sessCfg sessionOpts,
	attachCfg attachOpts,
	prompt, scriptPath string,
	tracker *usage.Tracker,
	modelName string,
	pricingRate usage.Pricing,
	opts []agent.Option,
	stop func(),
	send func(string),
) (*eventlog.Handle, string, bool, []agent.Option, error) {
	if sessCfg.continueSession && sessCfg.resumeSession != "" {
		return nil, "", false, opts, fmt.Errorf("cannot combine --continue and --resume; pass --continue for the latest project session or --resume [id] to select one")
	}
	handle, err := openEventlogIfEnabled(ctx, cfg, sessCfg, attachCfg, prompt, scriptPath, stop, send)
	if err != nil {
		return nil, "", false, opts, err
	}
	if handle == nil {
		return nil, defaultCLISessionID, false, opts, nil
	}
	opts = append(opts, agent.WithEventLog(handle))

	resuming := sessCfg.continueSession || sessCfg.resumeSession != ""
	attaching := attachCfg.Listen != "" || attachCfg.UnixSocket != ""
	if attaching && !resuming {
		if cfg == nil || !cfg.Attach.MultiSession.Enabled {
			compose.InstallSpendLedger(ctx, handle.Spend, tracker, cliSessionUserID, defaultCLISessionID)
		}
		return handle, defaultCLISessionID, false, opts, nil
	}

	sessionID, resumed, err := resolveCLISessionID(
		ctx, handle.Service, projectRoot,
		sessCfg.continueSession, sessCfg.resumeSession,
		os.Stdin, os.Stderr, runner.IsTerminal(os.Stdin),
	)
	if err != nil {
		_ = handle.Close()
		return nil, "", false, opts, err
	}
	opts = append(opts, agent.WithSession(cliSessionUserID, sessionID))
	restoreAndWireCLISpend(ctx, handle, cfg, tracker, sessionID, resumed, modelName, pricingRate, send)
	if !resumed {
		send(fmt.Sprintf("session: %s (fresh)", sessionID))
	}
	return handle, sessionID, resumed, opts, nil
}
