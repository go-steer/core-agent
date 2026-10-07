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

// Command dispatcher is the self-development soak's minimal dispatcher:
// the A7 subset (steps 0–3 and 7) of "The dispatcher" in
// docs/selfdev-soak-design.md. It hands a core-agent daemon one
// maintainer-queued issue from the private mirror at a time, then pushes
// the agent's local commits and opens the PR with the writer GitHub
// App's token, which never enters the daemon's pod. Its own design note
// is docs/selfdev-soak-dispatcher-design.md.
//
//	dispatcher --attach-url URL --token-file F --app-id N --app-key-file F \
//	    --worktrees-dir DIR --state-file F --commit-name NAME --commit-email EMAIL [--once]
//
// Every secret is read from a file: the attach bearer through
// childenv.TakeFile (the --token-file convention of #1266), the App key
// through a mode-checked PEM read. None is accepted on the command line
// or from the environment, and none is passed to a child process.
//
// Exit status: 0 when a polling run ends cleanly or --once opened a PR; 1
// on a runtime failure, or when --once's issue stopped or a signal ended
// it first; 2 on a usage error.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/go-steer/core-agent/v2/internal/attachclient"
	"github.com/go-steer/core-agent/v2/pkg/childenv"
)

// config is the parsed command line.
type config struct {
	Owner, Repo    string
	Maintainer     string
	Upstream       string
	APIURL         string
	GitRemote      string
	TagsRemote     string
	BaseBranch     string
	Poll           time.Duration
	MaxOpenPRs     int
	PrivateRepo    string
	WorktreesDir   string
	AgentsDir      string
	StateFile      string
	AttachURL      string
	TokenFile      string
	AppID          int64
	AppKeyFile     string
	InstallationID int64
	Identity       identity
	SessionTimeout time.Duration
	Settle         time.Duration
	StartTimeout   time.Duration
	Once           bool
}

func defaultStateDir() string { return filepath.Join(os.TempDir(), "selfdev-soak-dispatcher") }

// parseFlags reads args into a config. Errors are usage errors.
func parseFlags(args []string, stderr io.Writer) (config, error) {
	var c config
	var repo string
	fs := flag.NewFlagSet("dispatcher", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&repo, "repo", "mastersingh24/core-agent-selfdev", "the private mirror, owner/name")
	fs.StringVar(&c.Maintainer, "maintainer", "mastersingh24", "the only login whose issues become tasks (decision 6)")
	fs.StringVar(&c.Upstream, "upstream-repo", "go-steer/core-agent", "the repository every issue must link (decision 12)")
	fs.StringVar(&c.APIURL, "api-url", "https://api.github.com", "GitHub REST API root")
	fs.StringVar(&c.GitRemote, "git-remote", "", "the mirror's clone URL (default https://github.com/<repo>.git)")
	fs.StringVar(&c.TagsRemote, "tags-remote", "", "where release tags come from, fetched without a credential (default https://github.com/<upstream-repo>.git); only those that are ancestors of each base reach the working copy")
	fs.StringVar(&c.BaseBranch, "base-branch", "main", "the mirror branch work starts from and PRs target")
	fs.DurationVar(&c.Poll, "poll", 2*time.Minute, "poll interval")
	fs.IntVar(&c.MaxOpenPRs, "max-open-prs", 3, "take a new issue only while fewer agent PRs than this are open")
	fs.StringVar(&c.PrivateRepo, "private-repo", filepath.Join(defaultStateDir(), "mirror.git"), "the dispatcher-only bare repo; must NOT be on a volume the agent can write")
	fs.StringVar(&c.WorktreesDir, "worktrees-dir", "", "where per-issue working copies go, on the workspace volume the daemon sees (required)")
	fs.StringVar(&c.AgentsDir, "agents-dir", "", "the daemon's .agents directory, to read the session's plan artifact for the PR body (optional)")
	fs.StringVar(&c.StateFile, "state-file", "", "where the dispatcher remembers its active issue across restarts; must survive a pod restart (required)")
	fs.StringVar(&c.AttachURL, "attach-url", "", "the core-agent daemon's attach listener, e.g. http://core-agent:7777 (required)")
	fs.StringVar(&c.TokenFile, "token-file", "", "file holding the dispatcher's attach bearer token (required)")
	fs.Int64Var(&c.AppID, "app-id", 0, "the writer GitHub App's ID (required)")
	fs.StringVar(&c.AppKeyFile, "app-key-file", "", "the writer App's PEM private key (required)")
	fs.Int64Var(&c.InstallationID, "installation-id", 0, "the App's installation ID on the mirror (default: discovered)")
	fs.StringVar(&c.Identity.Name, "commit-name", "", "the soak's commit identity name (decision 16, required)")
	fs.StringVar(&c.Identity.Email, "commit-email", "", "the soak's commit identity email (decision 16, required)")
	fs.DurationVar(&c.SessionTimeout, "session-timeout", 4*time.Hour, "stop an issue whose session is still running after this long")
	fs.DurationVar(&c.Settle, "settle", 30*time.Second, "how long a session must stay idle after its last turn before it counts as done")
	fs.DurationVar(&c.StartTimeout, "start-timeout", 10*time.Minute, "stop an issue whose session never ends a turn within this long")
	fs.BoolVar(&c.Once, "once", false, "process exactly one issue end to end (to a PR or a stop), then exit: the A7 run")
	if err := fs.Parse(args); err != nil {
		return c, err
	}
	if fs.NArg() > 0 {
		return c, fmt.Errorf("unexpected arguments: %v", fs.Args())
	}
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return c, fmt.Errorf("--repo %q: want owner/name", repo)
	}
	c.Owner, c.Repo = owner, name
	if c.GitRemote == "" {
		c.GitRemote = "https://github.com/" + repo + ".git"
	}
	if c.TagsRemote == "" {
		c.TagsRemote = "https://github.com/" + c.Upstream + ".git"
	}
	return c, c.validate()
}

func (c config) validate() error {
	missing := []string{}
	for flagName, v := range map[string]string{
		"--worktrees-dir": c.WorktreesDir, "--attach-url": c.AttachURL,
		"--token-file": c.TokenFile, "--app-key-file": c.AppKeyFile, "--state-file": c.StateFile,
	} {
		if v == "" {
			missing = append(missing, flagName)
		}
	}
	if c.AppID <= 0 {
		missing = append(missing, "--app-id")
	}
	if len(missing) > 0 {
		return fmt.Errorf("required: %s", strings.Join(missing, ", "))
	}
	if c.MaxOpenPRs < 1 || c.Poll <= 0 || c.Settle <= 0 || c.StartTimeout <= 0 || c.SessionTimeout <= 0 {
		return errors.New("--max-open-prs must be at least 1 and every duration positive")
	}
	return validateIdentity(c.Identity)
}

// newDispatcher wires the components. tokens is the GitHub token source
// (the App in production).
func newDispatcher(c config, attachToken string, tokens tokenSource, log *slog.Logger) (*dispatcher, error) {
	st, err := loadState(c.StateFile)
	if err != nil {
		return nil, err
	}
	parsed, err := attachclient.ParseURL(c.AttachURL)
	if err != nil {
		return nil, fmt.Errorf("--attach-url: %w", err)
	}
	hc := &http.Client{Timeout: 60 * time.Second}
	return &dispatcher{
		cfg:    c,
		gh:     &ghClient{base: strings.TrimRight(c.APIURL, "/"), owner: c.Owner, repo: c.Repo, http: hc, tokens: tokens},
		tokens: tokens,
		daemon: &daemonClient{
			c: attachclient.New(parsed, attachToken, 30*time.Second), log: log,
			settle: c.Settle, startTimeout: c.StartTimeout, reconnect: 5 * time.Second,
		},
		git: &gitOps{
			bin: "git", remote: c.GitRemote, tagsRemote: c.TagsRemote, base: c.BaseBranch,
			privateDir: c.PrivateRepo, worktreesDir: c.WorktreesDir, id: c.Identity, inspect: inspectCopy,
		},
		log:     log,
		st:      st,
		skipped: map[int]string{},
	}, nil
}

// loadSecrets reads the attach token and the App key. Reading the token
// through TakeFile also makes the process non-dumpable, so neither secret
// can be read back out of /proc by a same-user process.
func loadSecrets(c config, log *slog.Logger) (string, *appTokenSource, error) {
	tok, protectErr, err := childenv.TakeFile(c.TokenFile)
	if err != nil {
		return "", nil, fmt.Errorf("--token-file: %w", err)
	}
	if protectErr != nil {
		log.Warn("could not make the process non-dumpable; a same-user process can read its memory", "err", protectErr)
	}
	key, err := loadAppKey(c.AppKeyFile)
	if err != nil {
		return "", nil, err
	}
	return tok, &appTokenSource{
		api: strings.TrimRight(c.APIURL, "/"), appID: c.AppID, key: key, installationID: c.InstallationID,
		owner: c.Owner, repo: c.Repo, http: &http.Client{Timeout: 30 * time.Second}, now: time.Now,
	}, nil
}

func main() {
	os.Exit(run(os.Args[1:], os.Stderr))
}

func run(args []string, stderr io.Writer) int {
	c, err := parseFlags(args, stderr)
	if err != nil {
		if !errors.Is(err, flag.ErrHelp) {
			fmt.Fprintf(stderr, "dispatcher: %v\n", err)
		}
		return 2
	}
	log := slog.New(slog.NewJSONHandler(stderr, nil)).With("component", "selfdev-soak-dispatcher")
	attachToken, tokens, err := loadSecrets(c, log)
	if err != nil {
		log.Error("startup", "err", err)
		return 2
	}
	d, err := newDispatcher(c, attachToken, tokens, log)
	if err != nil {
		log.Error("startup", "err", err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("starting", "repo", c.Owner+"/"+c.Repo, "maintainer", c.Maintainer, "once", c.Once,
		"poll", c.Poll.String(), "max_open_prs", c.MaxOpenPRs, "identity", c.Identity.String())
	return exitCode(d.runLoop(ctx), c.Once, log)
}

// runLoop runs the loop and folds a --once stop into an error.
func (d *dispatcher) runLoop(ctx context.Context) error {
	out, err := d.loop(ctx)
	if err == nil && out.Stopped {
		return fmt.Errorf("issue #%d: %w: %s", out.Issue, errStopped, out.Reason)
	}
	if err == nil && d.cfg.Once {
		d.log.Info("--once: done", "issue", out.Issue, "pr", out.PR, "url", out.PRURL)
	}
	return err
}

// exitCode maps the loop's end to a status. A signal ends a polling
// dispatcher cleanly (0), but ends a --once run without its PR, which a
// harness grading A7 must not read as success (1).
func exitCode(err error, once bool, log *slog.Logger) int {
	switch {
	case err == nil, errors.Is(err, context.Canceled) && !once:
		return 0
	default:
		log.Error("exiting", "err", err)
		return 1
	}
}
