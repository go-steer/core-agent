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

// wait_and_verify (#648) — poll a read-only tool until its result
// satisfies a condition, or until a bounded budget runs out.
//
// Closed-loop remediation ("apply the fix, wait for the state to
// converge, confirm it did") is the SRE headline this project keeps
// claiming. Before this tool, the only way to express the wait was a
// `bash sleep` — which the distroless recipes disable by design — and
// the only way to express the poll was N more model round-trips, each
// paying full prompt cost to ask "is it ready yet?". Recipes that
// couldn't do either just declared RESOLVED and hoped (#639).
//
// Three properties make this a primitive rather than a convenience:
//
//   - No shell. Pure Go, no subprocess, so it works in the distroless
//     images where `tools.disable: ["bash"]` is the whole point.
//
//   - Bounded, and bounded in the direction that matters. Wall clock
//     and attempt count are both capped by operator ceilings the model
//     cannot raise, and every individual poll runs under the wait's own
//     deadline, so a tool call that hangs is cut off with the budget
//     rather than hanging the turn. Token cost is bounded
//     by construction: N polls collapse into ONE tool result, so a
//     60-attempt wait costs the context what a single call costs.
//
//   - Read-only by construction. The poll target must be classified
//     read-only (IsReadOnlyTool, #460) or named in the operator's
//     poll_allow list. A loop that can call write_file 60 times is an
//     amplifier, not a verifier — one model-approved call must not
//     become sixty mutations.
//
// The result is also the evidence: attempts, timings, per-attempt
// match/error, and the final observation. "RESOLVED" backed by this
// payload is a claim a human can audit.
package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/itchyny/gojq"
	"google.golang.org/adk/model"
	adktool "google.golang.org/adk/tool"
	"google.golang.org/adk/tool/functiontool"
	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// WaitAndVerifyToolName is the canonical registered name.
const WaitAndVerifyToolName = "wait_and_verify"

// Defaults and hard bounds. The operator can lower the ceilings via
// config (tools.wait_and_verify); nothing can raise them past what
// they configure, and the model can't raise them at all.
const (
	defaultWaitInterval    = 5 * time.Second
	defaultWaitTimeout     = 60 * time.Second
	defaultWaitMaxTimeout  = 5 * time.Minute
	defaultWaitMaxAttempts = 60
	// minWaitInterval keeps a model that asks for interval_seconds: 0
	// from turning a bounded wait into a tight spin against someone's
	// API server. Clamped up rather than rejected: erring toward
	// slower costs a few seconds, erring toward faster costs a
	// rate-limit incident.
	minWaitInterval = 1 * time.Second
)

// Outcome values, closed set.
const (
	waitOutcomeVerified  = "verified"
	waitOutcomeTimeout   = "timeout"
	waitOutcomeExhausted = "attempts_exhausted"
	waitOutcomeCanceled  = "canceled"
)

// CatalogBinder is implemented by tools that need to call other tools.
// The full catalog isn't known until every tool is assembled and
// wrapped, so binding happens after construction rather than as a
// constructor argument — see BindCatalogs.
type CatalogBinder interface {
	BindCatalog(tools []adktool.Tool, toolsets []adktool.Toolset)
}

// CatalogBinders returns the tools in ts that want a catalog. Call it
// BEFORE the gate / serializer / instrumentation wrappers go on (they
// don't forward BindCatalog), and call BindCatalog on the results
// AFTER, with the wrapped slices — so a polled call goes through
// exactly the same layers a direct model call would.
func CatalogBinders(ts []adktool.Tool) []CatalogBinder {
	var out []CatalogBinder
	for _, t := range ts {
		if b, ok := t.(CatalogBinder); ok {
			out = append(out, b)
		}
	}
	return out
}

// BindCatalogs hands every binder in binders the assembled catalog.
func BindCatalogs(binders []CatalogBinder, ts []adktool.Tool, sets []adktool.Toolset) {
	for _, b := range binders {
		b.BindCatalog(ts, sets)
	}
}

// WaitAndVerifyOptions are the operator-set bounds, resolved from
// config by NewWaitAndVerifyTool.
type WaitAndVerifyOptions struct {
	// PollAllow names tools that may be polled even though the
	// runtime can't classify them read-only. This is how MCP tools
	// become pollable: ADK's MCP adapter does not surface the
	// server's readOnlyHint annotation, so every MCP tool classifies
	// as mutating (the fail-safe default) and would otherwise be
	// refused. Listing one here is an operator assertion that the
	// tool observes without mutating.
	PollAllow []string
	// MaxTimeout caps timeout_seconds. Zero means defaultWaitMaxTimeout.
	MaxTimeout time.Duration
	// MaxAttempts caps max_attempts. Zero means defaultWaitMaxAttempts.
	MaxAttempts int
	// BashRegistered reports whether this build registered the `bash`
	// tool, so the description can drop its "PREFERRED over `bash
	// sleep`" comparison when there is no shell to prefer it over.
	// Description-only; it has no effect on what may be polled.
	//
	// Unlike the gate-carried catalog the other tools consult, this is
	// an explicit field: wait_and_verify is constructed without a gate
	// (see NewWaitAndVerifyTool), and threading one in just to read a
	// description flag would be a wider change than the text is worth.
	// tools.Build sets it from gate.HasTool("bash"); a host wiring the
	// tool by hand leaves it false and gets the shell-free wording.
	BashRegistered bool
}

// neverPollable names tools that stay unpollable no matter what the
// operator allows.
//
//   - wait_and_verify itself: a waiter polling a waiter multiplies the
//     budget by the nesting depth, and the outer bound stops meaning
//     anything.
//   - ask_user: read-only by dispatch class, but it blocks on a HUMAN.
//     Sixty prompts on a timer is not a verification loop.
var neverPollable = map[string]bool{
	WaitAndVerifyToolName: true,
	"ask_user":            true,
}

type waitAndVerifyArgs struct {
	Tool              string  `json:"tool" jsonschema:"name of the tool to poll. Must be a read-only tool (or one the operator allow-listed for polling); mutating tools are refused."`
	ArgsJSON          string  `json:"args_json,omitempty" jsonschema:"arguments for the polled tool, as a JSON object string. Example: '{\"namespace\":\"prod\",\"name\":\"api\"}'. The SAME arguments are used for every attempt. Omit for a no-argument tool."`
	ExpectJQ          string  `json:"expect_jq,omitempty" jsonschema:"jq expression evaluated against the polled tool's result. The condition is satisfied when it yields a truthy value (anything but false/null/no output). Example: '.pods | all(.status == \"Running\")'. PREFERRED over the substring checks."`
	ExpectContains    string  `json:"expect_contains,omitempty" jsonschema:"substring that must appear in the JSON-serialized result."`
	ExpectNotContains string  `json:"expect_not_contains,omitempty" jsonschema:"substring that must NOT appear in the JSON-serialized result."`
	IntervalSeconds   float64 `json:"interval_seconds,omitempty" jsonschema:"seconds to wait between attempts. Default 5, minimum 1."`
	TimeoutSeconds    float64 `json:"timeout_seconds,omitempty" jsonschema:"total wall-clock budget in seconds. Default 60; the operator sets the ceiling and exceeding it is an error, not a silent clamp."`
	MaxAttempts       int     `json:"max_attempts,omitempty" jsonschema:"maximum number of polls. Default 60; capped by the operator."`
}

// waitObservation is one poll, kept small on purpose: this list is the
// audit trail, not a second copy of the payload.
type waitObservation struct {
	Attempt   int     `json:"attempt"`
	AtSeconds float64 `json:"at_seconds"`
	Matched   bool    `json:"matched"`
	Error     string  `json:"error,omitempty"`
}

type waitAndVerifyResult struct {
	// Verified is the answer. Everything else is evidence for it.
	Verified bool   `json:"verified"`
	Outcome  string `json:"outcome"`
	Tool     string `json:"tool"`
	// Condition restates what was actually checked, so a transcript
	// reader doesn't have to go find the call arguments.
	Condition string `json:"condition"`
	Attempts  int    `json:"attempts"`
	// IntervalSeconds is the EFFECTIVE interval after clamping, which
	// may not be what was requested.
	IntervalSeconds float64           `json:"interval_seconds"`
	ElapsedSeconds  float64           `json:"elapsed_seconds"`
	LastResult      string            `json:"last_result,omitempty"`
	LastError       string            `json:"last_error,omitempty"`
	Observations    []waitObservation `json:"observations"`
}

// waitVerifier holds the late-bound catalog and the resolved bounds.
type waitVerifier struct {
	cfg  *config.Config
	opts WaitAndVerifyOptions
	// minInterval is a field rather than the constant so tests can
	// drive the loop at millisecond speed.
	minInterval time.Duration
	allow       map[string]bool

	mu       sync.RWMutex
	tools    []adktool.Tool
	toolsets []adktool.Toolset
}

// waitAndVerifyTool is the registered tool. It exists as a wrapper
// only so agent wiring can find BindCatalog: functiontool's concrete
// type is unexported, and the catalog isn't knowable at construction
// time. Every ADK-facing method is promoted from the embedded tool.
type waitAndVerifyTool struct {
	callableTool
	v *waitVerifier
}

// callableTool is the full set of methods ADK needs from a tool,
// gathered so the embedding above promotes all of them (adktool.Tool
// alone would leave Run and Declaration behind).
type callableTool interface {
	adktool.Tool
	Declaration() *genai.FunctionDeclaration
	Run(ctx adktool.Context, args any) (map[string]any, error)
	ProcessRequest(ctx adktool.Context, req *model.LLMRequest) error
}

// ReadOnlyHint: the waiter mutates nothing itself, and refuses to poll
// anything that does — so it is safe to dispatch concurrently with
// other read-only calls (#460).
func (w *waitAndVerifyTool) ReadOnlyHint() bool { return true }

func (w *waitAndVerifyTool) BindCatalog(ts []adktool.Tool, sets []adktool.Toolset) {
	w.v.mu.Lock()
	defer w.v.mu.Unlock()
	w.v.tools = ts
	w.v.toolsets = sets
}

// NewWaitAndVerifyTool builds the tool. The returned value implements
// CatalogBinder and is inert — every call fails with "no tool catalog"
// — until something binds a catalog to it.
func NewWaitAndVerifyTool(cfg *config.Config, opts WaitAndVerifyOptions) (adktool.Tool, error) {
	if cfg == nil {
		return nil, errors.New("tools: wait_and_verify: cfg is required")
	}
	if opts.MaxTimeout <= 0 {
		opts.MaxTimeout = defaultWaitMaxTimeout
	}
	if opts.MaxAttempts <= 0 {
		opts.MaxAttempts = defaultWaitMaxAttempts
	}
	v := &waitVerifier{
		cfg:         cfg,
		opts:        opts,
		minInterval: minWaitInterval,
		allow:       make(map[string]bool, len(opts.PollAllow)),
	}
	for _, n := range opts.PollAllow {
		if n = strings.TrimSpace(n); n != "" {
			v.allow[n] = true
		}
	}
	inner, err := functiontool.New(functiontool.Config{
		Name: WaitAndVerifyToolName,
		Description: "Poll a read-only tool until its result satisfies a condition, or until a bounded budget expires. " +
			"Use this for fix-and-verify: after applying a change, poll the tool that observes the state (pod status, endpoint health, a file's contents) until it converges. " +
			"PREFERRED over polling yourself across several turns (which pays full prompt cost per attempt — this collapses every attempt into one result)." +
			whenTool(opts.BashRegistered, " Also PREFERRED over `bash sleep` + a re-check.") +
			" Returns verified plus the attempt-by-attempt evidence; cite it rather than asserting a fix worked.",
	}, v.run)
	if err != nil {
		return nil, fmt.Errorf("tools: wait_and_verify: %w", err)
	}
	callable, ok := inner.(callableTool)
	if !ok {
		return nil, fmt.Errorf("tools: wait_and_verify: functiontool returned %T, which is not callable", inner)
	}
	return &waitAndVerifyTool{callableTool: callable, v: v}, nil
}

// resolve finds the named tool in the bound catalog and checks that it
// may be polled. Toolsets resolve lazily (an MCP toolset fetches from
// its server), so this happens once per wait_and_verify call rather
// than once per process.
func (v *waitVerifier) resolve(ctx adktool.Context, name string) (runnableTool, error) {
	v.mu.RLock()
	tools := v.tools
	toolsets := v.toolsets
	v.mu.RUnlock()

	if len(tools) == 0 && len(toolsets) == 0 {
		return nil, fmt.Errorf("%s: no tool catalog is bound to this agent, so there is nothing to poll", WaitAndVerifyToolName)
	}

	var pollable []string
	var found adktool.Tool
	consider := func(t adktool.Tool) {
		if t == nil {
			return
		}
		ok := v.mayPoll(t)
		if ok {
			pollable = append(pollable, t.Name())
		}
		if t.Name() == name {
			found = t
		}
	}
	for _, t := range tools {
		consider(t)
	}
	for _, ts := range toolsets {
		if ts == nil {
			continue
		}
		sub, err := ts.Tools(ctx)
		if err != nil {
			// One unreachable MCP server must not make every other
			// tool unpollable.
			continue
		}
		for _, t := range sub {
			consider(t)
		}
	}

	if found == nil {
		sort.Strings(pollable)
		return nil, fmt.Errorf("%s: unknown tool %q; pollable tools are: %s", WaitAndVerifyToolName, name, strings.Join(pollable, ", "))
	}
	if !v.mayPoll(found) {
		return nil, fmt.Errorf("%s: refusing to poll %q: it is not classified read-only. "+
			"Polling repeats the call up to max_attempts times, so a mutating tool would be applied that many times. "+
			"If it really only observes state, the operator can add it to tools.wait_and_verify.poll_allow", WaitAndVerifyToolName, name)
	}
	rn, ok := found.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("%s: tool %q is not callable", WaitAndVerifyToolName, name)
	}
	return rn, nil
}

// mayPoll is the by-construction half of the safety claim: read-only
// by the runtime's own classifier, or an explicit operator assertion.
// Never both-ways for the neverPollable set.
func (v *waitVerifier) mayPoll(t adktool.Tool) bool {
	name := t.Name()
	if neverPollable[name] {
		return false
	}
	return IsReadOnlyTool(t) || v.allow[name]
}

// waitCondition is the parsed, pre-compiled predicate. Compiling
// before the loop means a bad jq expression costs one error instead of
// max_attempts of them.
type waitCondition struct {
	jq          *gojq.Query
	jqSrc       string
	contains    string
	notContains string
}

func (c waitCondition) describe() string {
	var parts []string
	if c.jqSrc != "" {
		parts = append(parts, "expect_jq="+c.jqSrc)
	}
	if c.contains != "" {
		parts = append(parts, fmt.Sprintf("expect_contains=%q", c.contains))
	}
	if c.notContains != "" {
		parts = append(parts, fmt.Sprintf("expect_not_contains=%q", c.notContains))
	}
	return strings.Join(parts, " AND ")
}

// match reports whether the polled result satisfies every configured
// clause. It takes the SERIALIZED result rather than the raw map: the
// substring clauses match the same text the model will see, and gojq
// needs plain JSON types anyway, so one JSON round-trip serves both.
// The returned error means the expression cannot evaluate against this
// result's shape — waiting will not fix that, so the caller aborts
// instead of burning the budget.
func (c waitCondition) match(ctx context.Context, serialized string) (bool, error) {
	if c.contains != "" && !strings.Contains(serialized, c.contains) {
		return false, nil
	}
	if c.notContains != "" && strings.Contains(serialized, c.notContains) {
		return false, nil
	}
	if c.jq == nil {
		return true, nil
	}
	// gojq requires plain JSON types; a tool result map can hold
	// anything Go. Parsing the serialized form normalizes it.
	var input any
	if err := json.Unmarshal([]byte(serialized), &input); err != nil {
		return false, fmt.Errorf("normalize result for expect_jq: %w", err)
	}
	// RunWithContext, not Run: a jq expression can loop forever
	// (`repeat(1)`, a huge `range`), and evaluation happens OUTSIDE the
	// poll's own deadline. Without the context, one crafted expression
	// would hang the turn — which would make "bounded" a claim rather
	// than a property.
	iter := c.jq.RunWithContext(ctx, input)
	matched := false
	for {
		out, ok := iter.Next()
		if !ok {
			break
		}
		if err, isErr := out.(error); isErr {
			return false, fmt.Errorf("expect_jq %q: %w", c.jqSrc, err)
		}
		if truthy(out) {
			matched = true
		}
	}
	return matched, nil
}

// truthy follows jq: everything except false and null.
func truthy(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	default:
		return true
	}
}

func parseWaitCondition(in waitAndVerifyArgs) (waitCondition, error) {
	c := waitCondition{contains: in.ExpectContains, notContains: in.ExpectNotContains, jqSrc: in.ExpectJQ}
	if c.jqSrc == "" && c.contains == "" && c.notContains == "" {
		return c, fmt.Errorf("%s: a condition is required — set expect_jq (preferred), expect_contains, or expect_not_contains. Without one this is just a sleep", WaitAndVerifyToolName)
	}
	if c.jqSrc != "" {
		q, err := gojq.Parse(c.jqSrc)
		if err != nil {
			return c, fmt.Errorf("%s: parse expect_jq: %w", WaitAndVerifyToolName, err)
		}
		c.jq = q
	}
	return c, nil
}

// bounds are the resolved per-call budget.
type waitBounds struct {
	interval    time.Duration
	timeout     time.Duration
	maxAttempts int
}

// resolveBounds applies defaults, clamps the interval UP to the floor,
// and refuses anything past the operator's ceilings.
//
// The asymmetry is deliberate. Clamping the interval up is the safe
// direction and reporting the effective value keeps it honest.
// Clamping a timeout DOWN would have the tool report "I waited" for a
// budget it silently shortened — the exact class of unenforced claim
// this milestone exists to remove — so that is an error the model can
// see and correct.
func (v *waitVerifier) resolveBounds(in waitAndVerifyArgs) (waitBounds, error) {
	b := waitBounds{interval: defaultWaitInterval, timeout: defaultWaitTimeout, maxAttempts: v.opts.MaxAttempts}

	if in.IntervalSeconds < 0 {
		return b, fmt.Errorf("%s: interval_seconds=%v must be >= 0 (0 means the default)", WaitAndVerifyToolName, in.IntervalSeconds)
	}
	if in.IntervalSeconds > 0 {
		b.interval = time.Duration(in.IntervalSeconds * float64(time.Second))
	}
	if b.interval < v.minInterval {
		b.interval = v.minInterval
	}

	if in.TimeoutSeconds < 0 {
		return b, fmt.Errorf("%s: timeout_seconds=%v must be >= 0 (0 means the default)", WaitAndVerifyToolName, in.TimeoutSeconds)
	}
	// An operator ceiling below the built-in default lowers the
	// DEFAULT; it does not make every call an error. Only an explicit
	// over-ceiling request is refused — the refusal exists to stop the
	// model claiming a budget it wasn't given, and a model that asked
	// for nothing claimed nothing.
	if b.timeout > v.opts.MaxTimeout {
		b.timeout = v.opts.MaxTimeout
	}
	if in.TimeoutSeconds > 0 {
		requested := time.Duration(in.TimeoutSeconds * float64(time.Second))
		if requested > v.opts.MaxTimeout {
			return b, fmt.Errorf("%s: timeout_seconds=%v exceeds the operator's ceiling of %v seconds (tools.wait_and_verify.max_timeout_seconds)",
				WaitAndVerifyToolName, requested.Seconds(), v.opts.MaxTimeout.Seconds())
		}
		b.timeout = requested
	}

	if in.MaxAttempts < 0 {
		return b, fmt.Errorf("%s: max_attempts=%d must be >= 0 (0 means the default)", WaitAndVerifyToolName, in.MaxAttempts)
	}
	if in.MaxAttempts > 0 {
		if in.MaxAttempts > v.opts.MaxAttempts {
			return b, fmt.Errorf("%s: max_attempts=%d exceeds the operator's ceiling of %d (tools.wait_and_verify.max_attempts)",
				WaitAndVerifyToolName, in.MaxAttempts, v.opts.MaxAttempts)
		}
		b.maxAttempts = in.MaxAttempts
	}
	return b, nil
}

// deadlineToolContext narrows a tool.Context to the wait's deadline —
// normally earlier than the turn's — so a polled tool that hangs is
// cut off when the budget expires instead of hanging the turn. Only the
// context.Context half is overridden; everything else is the real
// tool context, so the polled tool still sees its invocation, its
// actions, and its confirmation handler.
type deadlineToolContext struct {
	adktool.Context
	ctx context.Context
}

func (d deadlineToolContext) Deadline() (time.Time, bool) { return d.ctx.Deadline() }
func (d deadlineToolContext) Done() <-chan struct{}       { return d.ctx.Done() }
func (d deadlineToolContext) Err() error                  { return d.ctx.Err() }
func (d deadlineToolContext) Value(key any) any           { return d.ctx.Value(key) }

func (v *waitVerifier) run(ctx adktool.Context, in waitAndVerifyArgs) (waitAndVerifyResult, error) {
	if strings.TrimSpace(in.Tool) == "" {
		return waitAndVerifyResult{}, fmt.Errorf("%s: tool is required", WaitAndVerifyToolName)
	}
	cond, err := parseWaitCondition(in)
	if err != nil {
		return waitAndVerifyResult{}, err
	}
	bounds, err := v.resolveBounds(in)
	if err != nil {
		return waitAndVerifyResult{}, err
	}
	args := map[string]any{}
	if s := strings.TrimSpace(in.ArgsJSON); s != "" {
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			return waitAndVerifyResult{}, fmt.Errorf("%s: args_json must be a JSON object: %w", WaitAndVerifyToolName, err)
		}
	}
	target, err := v.resolve(ctx, in.Tool)
	if err != nil {
		return waitAndVerifyResult{}, err
	}

	parent := context.Context(ctx)
	if parent == nil {
		parent = context.Background()
	}

	start := time.Now()
	deadline := start.Add(bounds.timeout)
	res := waitAndVerifyResult{
		Tool:            in.Tool,
		Condition:       cond.describe(),
		IntervalSeconds: bounds.interval.Seconds(),
		Outcome:         waitOutcomeTimeout,
	}
	caps := capsFor(v.cfg, WaitAndVerifyToolName, 16*1024, 400)

	for attempt := 1; attempt <= bounds.maxAttempts; attempt++ {
		res.Attempts = attempt
		obs := waitObservation{Attempt: attempt, AtSeconds: roundSeconds(time.Since(start))}

		// One deadline covers the call AND the condition evaluation:
		// both are ways for a single attempt to outlive the budget.
		ok, serialized, callErr, matchErr := func() (bool, string, error, error) {
			pollCtx, cancel := context.WithDeadline(parent, deadline)
			defer cancel()
			out, err := target.Run(deadlineToolContext{Context: ctx, ctx: pollCtx}, args)
			if err != nil {
				return false, "", err, nil
			}
			s := serializeToolResult(out)
			matched, mErr := cond.match(pollCtx, s)
			if mErr != nil && pollCtx.Err() != nil {
				// The budget ran out mid-evaluation (a slow or runaway
				// expression), not a bad expression. Report it the way
				// any other attempt that didn't finish in time is
				// reported — as evidence — rather than as a tool
				// failure, and let the loop's own bounds end the wait.
				return false, s, mErr, nil
			}
			return matched, s, nil, mErr
		}()

		if callErr != nil {
			// A transient failure is exactly what a wait is for: the
			// API server is rolling, the endpoint isn't serving yet.
			// Record it and keep going; if it never clears, it lands
			// in last_error at timeout.
			obs.Error = callErr.Error()
			res.LastError = callErr.Error()
		} else {
			res.LastError = ""
			res.LastResult = Truncate(serialized, caps.bytes, caps.lines)
			if matchErr != nil {
				// The expression can't evaluate against this shape.
				// Waiting won't change that, so fail loudly on the
				// first attempt rather than after the full budget.
				return res, fmt.Errorf("%s: %w", WaitAndVerifyToolName, matchErr)
			}
			obs.Matched = ok
			if ok {
				res.Observations = append(res.Observations, obs)
				res.Verified = true
				res.Outcome = waitOutcomeVerified
				res.ElapsedSeconds = roundSeconds(time.Since(start))
				return res, nil
			}
		}
		res.Observations = append(res.Observations, obs)

		if attempt == bounds.maxAttempts {
			res.Outcome = waitOutcomeExhausted
			break
		}
		// Stop when the next attempt would land past the deadline —
		// sleeping into a budget we've already spent just delays the
		// same answer.
		if time.Now().Add(bounds.interval).After(deadline) {
			res.Outcome = waitOutcomeTimeout
			break
		}
		select {
		case <-parent.Done():
			res.Outcome = waitOutcomeCanceled
			res.ElapsedSeconds = roundSeconds(time.Since(start))
			return res, nil
		case <-time.After(bounds.interval):
		}
	}
	res.ElapsedSeconds = roundSeconds(time.Since(start))
	return res, nil
}

// serializeToolResult renders a tool result for substring matching and
// for the last_result field. A result that won't marshal (a channel,
// a func) falls back to Go formatting rather than failing the wait.
func serializeToolResult(out map[string]any) string {
	b, err := json.Marshal(out)
	if err != nil {
		return fmt.Sprintf("%v", out)
	}
	return string(b)
}

func roundSeconds(d time.Duration) float64 {
	return float64(d.Milliseconds()) / 1000.0
}

// WaitAndVerifyOptionsFromConfig maps the operator's config block onto
// the runtime bounds.
func WaitAndVerifyOptionsFromConfig(cfg *config.Config) WaitAndVerifyOptions {
	if cfg == nil {
		return WaitAndVerifyOptions{}
	}
	wv := cfg.Tools.WaitAndVerify
	opts := WaitAndVerifyOptions{PollAllow: wv.PollAllow, MaxAttempts: wv.MaxAttempts}
	if wv.MaxTimeoutSeconds > 0 {
		opts.MaxTimeout = time.Duration(wv.MaxTimeoutSeconds) * time.Second
	}
	return opts
}
