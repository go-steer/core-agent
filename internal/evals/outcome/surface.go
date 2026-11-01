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

package outcome

// The tool surface the O tier shows the model.
//
// §4 settled which one: k8s-lookout's read-only MCP server, over stdio,
// against the throwaway kind cluster. The hosted GKE MCP surface was
// ruled out by measurement rather than by cost — testdata/evals/intents.yaml
// has no rows for it, so every intent_satisfied check in the corpus would
// be vacuous and the aggregate would red on a fact about the table.
//
// Three things this file decides that §4 left to the implementation.
//
// WHICH TOOLS. Not lookout's whole surface: `--profile=all` is 34 tools
// and 160 KB of JSON schema advertised on every model call, which is
// most of the tier's bill for tools the corpus cannot score. Not a
// hand-picked list either, which is the same drift a second images list
// would be. The selection is *the intent table's own tool names*: the
// intent layer is what grades the run, so the surface the model is shown
// is exactly the surface the grader can read. Eleven tools, ~53 KB. If
// the table grows a tool, the surface grows with it and nothing here
// changes.
//
// The model still has a real choice among eleven, which is the point --
// narrowing to the one tool the admitted roster needs would make
// intent_satisfied a tautology.
//
// WHY THE CONFIG IS BUILT IN GO rather than read from a committed
// mcp.json. The kubeconfig path is generated per run, so a committed
// file would have to reach it through ${MAST_OUTCOME_KUBECONFIG} — which
// means putting a kubeconfig path into the runner's own environment,
// where every other child it launches can also see it. Handing the
// server one literal path and an otherwise empty environment is the
// narrower thing, and it still goes through the same [mcp.NewToolset]
// the daemon uses, so the stdio launch, the env scoping and the refusal
// of server-initiated input are all the shipped code paths.
//
// WHY THE CHILD GETS NO HOME. EnvMode "clean" with PATH as the only
// passthrough. lookout resolves a cluster through clientcmd, which falls
// back to ~/.kube/config when KUBECONFIG is unset — so if the KUBECONFIG
// we set ever failed to apply, a child with HOME would silently read the
// operator's real cluster and a whole board would grade against it.
// Without HOME there is no fallback to find, and the run fails saying so.
// That is the same reasoning as cluster.go's envWithoutKubeconfig, from
// the other side.

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/mcptoolset"

	"github.com/go-steer/core-agent/v3/internal/evals"
	"github.com/go-steer/core-agent/v3/internal/version"
)

// DefaultLookout is the binary name looked up on PATH when no path is
// configured. Pinned by version at the install site, not here: a gate
// whose tool surface floats reds for reasons no PR author can act on.
const DefaultLookout = "lookout"

// surfaceProbeTimeout bounds the pre-flight `--list-tools` and
// `--version` reads. Both are local and instant; this only stops a
// wedged binary from eating the pass's wall-clock ceiling.
const surfaceProbeTimeout = 30 * time.Second

// Surface is a validated lookout MCP configuration plus what the
// pre-flight learned about it.
//
// It does not hold a toolset. Each agent run mints its own through
// [Surface.Toolset], which launches its own stdio child — see the note
// there for why that is not wasteful.
type Surface struct {
	// Binary is the resolved absolute path, and Version is what it
	// reports. Both go on the board: "which tool surface produced this"
	// is the first question about any outcome number.
	Binary  string
	Version string
	// Tools are the tool names the model is shown, sorted.
	Tools []string

	kubeconfig string
}

// SurfaceConfig configures [NewSurface].
type SurfaceConfig struct {
	// Binary is the lookout executable. Defaults to [DefaultLookout],
	// resolved on PATH.
	Binary string
	// Cluster is the throwaway cluster the server reads. Required.
	Cluster *Cluster
	// Table is the intent table, which is also the tool selection.
	Table evals.IntentTable
}

// NewSurface resolves lookout, decides the tool selection, and proves
// the selection before anything metered runs.
//
// The proof is a probes-before-run of the same shape the fixture
// provisioner does: `lookout mcp --tools=<selection> --list-tools`
// enumerates what the server would advertise, and a selected name that
// does not come back is a refusal here rather than a tool the model
// never sees and an intent check that quietly measures nothing.
func NewSurface(ctx context.Context, cfg SurfaceConfig) (*Surface, error) {
	if cfg.Cluster == nil {
		return nil, fmt.Errorf("outcome: surface needs a cluster")
	}
	selected := selectTools(cfg.Table)
	if len(selected) == 0 {
		return nil, fmt.Errorf("outcome: the intent table names no lookout tools, so the model would be shown nothing and every intent check would measure the empty surface")
	}
	// Re-verified before the kubeconfig is handed to a process that has
	// no --context flag of its own. lookout will resolve this file's
	// current-context, and the only thing that makes that safe is that
	// the file has been proven to describe exactly one context and that
	// it is ours.
	if err := cfg.Cluster.verifyIsolation(); err != nil {
		return nil, err
	}

	name := cfg.Binary
	if name == "" {
		name = DefaultLookout
	}
	bin, err := exec.LookPath(name)
	if err != nil {
		return nil, fmt.Errorf("outcome: %s: %w — install a pinned build, e.g. `go install github.com/go-steer/k8s-lookout/cmd/lookout@%s`", name, err, PinnedLookout)
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		return nil, err
	}

	version, err := probe(ctx, bin, "--version")
	if err != nil {
		return nil, fmt.Errorf("outcome: %s --version: %w", bin, err)
	}

	sel := strings.Join(selected, ",")
	listed, err := probe(ctx, bin, "mcp", "--tools="+sel, "--list-tools")
	if err != nil {
		return nil, fmt.Errorf("outcome: %s mcp --tools=%s --list-tools: %w", bin, sel, err)
	}
	if missing := notAdvertised(selected, listed); len(missing) > 0 {
		return nil, fmt.Errorf("outcome: %s advertises no %s — the intent table names %d lookout tools and this build has %d of them, so those intents are unreachable and their checks would measure nothing",
			filepath.Base(bin), strings.Join(missing, ", "), len(selected), len(selected)-len(missing))
	}

	return &Surface{
		Binary:     bin,
		Version:    strings.TrimSpace(firstLine(version)),
		Tools:      selected,
		kubeconfig: cfg.Cluster.Kubeconfig,
	}, nil
}

// PinnedLookout is the lookout release the O tier is measured against,
// and the one CI installs.
//
// A pin rather than @latest, for the same reason the corpus pins a model
// id: a gate that reds because the tool surface changed under it teaches
// PR authors that the gate is noise, and a gate people have learned to
// ignore is worse than no gate. Bumping it is a reviewable diff whose
// board delta is the evidence for or against the bump.
const PinnedLookout = "v0.23.0"

// Toolset mints a toolset for one agent run. The MCP session — and so
// the stdio child — is established lazily on the first tool call, so a
// run whose model never calls a tool costs no process.
//
// One child per concurrent worker rather than one shared across the
// pass, which costs a process and buys not having to reason about
// whether lookout's server is safe under concurrent calls. The children
// exit on stdin EOF when the runner does; the ADK toolset exposes no
// Close, which is the reason this is worth stating rather than assuming.
func (s *Surface) Toolset(ctx context.Context, name string) (tool.Toolset, error) {
	sel := strings.Join(s.Tools, ",")
	cmd := (*Cluster)(nil).command(ctx, s.Binary, "mcp", "--tools="+sel)
	env := []string{"KUBECONFIG=" + s.kubeconfig}
	if p, ok := os.LookupEnv("PATH"); ok {
		env = append(env, "PATH="+p)
	}
	cmd.Env = env
	client := mcpsdk.NewClient(
		&mcpsdk.Implementation{Name: "core-agent", Version: version.Version},
		&mcpsdk.ClientOptions{
			MultiRoundTrip: &mcpsdk.MultiRoundTripOptions{Disabled: true},
			Capabilities:   &mcpsdk.ClientCapabilities{},
		},
	)
	ts, err := mcptoolset.New(mcptoolset.Config{
		Client:    client,
		Transport: &mcpsdk.CommandTransport{Command: cmd},
	})
	if err != nil {
		return nil, fmt.Errorf("outcome: %q: construct toolset: %w", name, err)
	}
	return namedToolset{name: name, Toolset: ts}, nil
}

type namedToolset struct {
	tool.Toolset
	name string
}

func (n namedToolset) Name() string { return n.name }

// selectTools is the intent table's tool names, sorted. This is the
// whole selection policy; see the file header for why it is not a list.
func selectTools(tbl evals.IntentTable) []string {
	out := make([]string, 0, len(tbl.LookoutTools))
	for name := range tbl.LookoutTools {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// notAdvertised is the selected names `--list-tools` did not print back.
func notAdvertised(selected []string, listed string) []string {
	seen := map[string]bool{}
	sc := bufio.NewScanner(strings.NewReader(listed))
	for sc.Scan() {
		// Each row is `<name> <schema-bytes> <summary>`, and the last
		// row is a total whose first field is a count. Rejecting a
		// numeric first field rather than the last line: a pre-flight
		// that will accept anything the output happens to contain is a
		// pre-flight that can be satisfied by a name no tool has.
		f := firstField(sc.Text())
		if f == "" {
			continue
		}
		if _, err := strconv.Atoi(f); err == nil {
			continue
		}
		seen[f] = true
	}
	var missing []string
	for _, want := range selected {
		if !seen[want] {
			missing = append(missing, want)
		}
	}
	return missing
}

// probe runs a local, instant lookout subcommand and returns its output.
//
// Through [Cluster.command] on a nil receiver, which is the package's
// one sanctioned way to start a process (see TestOneExecPath). Neither
// of these subcommands reads a cluster, so the receiver has nothing to
// contribute — but going through it anyway is what keeps the guard's
// claim literally true, and it gets KUBECONFIG stripped for free, which
// is the right environment for a probe that must not resolve one.
func probe(ctx context.Context, bin string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, surfaceProbeTimeout)
	defer cancel()
	out, err := run((*Cluster)(nil).command(ctx, bin, args...))
	if err != nil {
		return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(out))
	}
	return out, nil
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
