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

package approvereval

import (
	"context"
	"path/filepath"
	"sync"
	"time"

	"google.golang.org/genai"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// Workspace is the project root every case's gate is scoped to. Paths in
// a case are relative to it; an absolute path outside it is a
// path-scope request.
const Workspace = "/workspace"

// Outcome is what the gate did with one case.
type Outcome struct {
	// ApproverCalls is how many times the approver was asked.
	ApproverCalls int
	// Audit is the gate's record of the approver's verdict, after its
	// own rules (an unusable answer escalates), or nil when the approver
	// was not asked.
	Audit *permissions.ApproverAudit
	// Prompted is whether the call reached the person's prompt.
	Prompted bool
	// Allowed is whether the call was allowed to run.
	Allowed bool
	// Usage is the approver's reported token usage, summed.
	InputTokens, OutputTokens int32
	// Elapsed is the wall time of the gate check.
	Elapsed time.Duration
}

// countingApprover counts calls to the approver under test.
type countingApprover struct {
	inner permissions.Approver
	mu    sync.Mutex
	n     int
}

func (c *countingApprover) Judge(ctx context.Context, req permissions.ApproverRequest) (permissions.Verdict, error) {
	c.mu.Lock()
	c.n++
	c.mu.Unlock()
	return c.inner.Judge(ctx, req)
}

// recordingPrompter stands in for the person: it records that it was
// asked and refuses, so nothing past the prompt runs.
type recordingPrompter struct{ asked bool }

func (p *recordingPrompter) AskApproval(context.Context, permissions.PromptRequest) (permissions.Decision, error) {
	p.asked = true
	return permissions.DecisionDeny, nil
}

// caseContext is the turn the case runs in, as the agent would stamp it.
type caseContext struct {
	task   string
	recent []permissions.ApproverCall
	mu     sync.Mutex
	audit  *permissions.ApproverAudit
	in     int32
	out    int32
}

func (c *caseContext) Task() string                            { return c.task }
func (c *caseContext) RecentCalls() []permissions.ApproverCall { return c.recent }
func (c *caseContext) CeilingReached() bool                    { return false }

func (c *caseContext) Bill(_ string, u *genai.GenerateContentResponseUsageMetadata) {
	if u == nil {
		return
	}
	c.mu.Lock()
	c.in += u.PromptTokenCount
	c.out += u.CandidatesTokenCount
	c.mu.Unlock()
}

func (c *caseContext) Audit(a permissions.ApproverAudit) {
	c.mu.Lock()
	c.audit = &a
	c.mu.Unlock()
}

// Run puts one case to a ModeAuto gate whose approver is a. The gate is
// built fresh per case, so nothing one case's verdict leaves behind (the
// turn's refusal memory) reaches another.
func Run(ctx context.Context, a permissions.Approver, c Case) (Outcome, error) {
	pol, err := permissions.NewPolicy(c.Eligible, nil)
	if err != nil {
		return Outcome{}, err
	}
	scope, err := permissions.NewPathScope(Workspace, "", nil)
	if err != nil {
		return Outcome{}, err
	}
	counted := &countingApprover{inner: a}
	prompter := &recordingPrompter{}
	g := permissions.New(permissions.Options{
		Mode:            permissions.ModeAuto,
		Scope:           scope,
		Prompter:        prompter,
		Approver:        counted,
		AutoEligible:    pol,
		ApprovalTimeout: time.Minute,
	})
	turn := &caseContext{task: c.Task}
	for _, rc := range c.RecentCalls {
		turn.recent = append(turn.recent, permissions.ApproverCall{ToolName: rc.Tool, Detail: rc.Detail})
	}
	ctx = permissions.WithApproverContext(ctx, turn)
	if c.Source != "" {
		ctx = permissions.WithSubagentSource(ctx, c.Source)
	}

	start := time.Now()
	var checkErr error
	switch c.Call.Check {
	case CheckBash:
		checkErr = g.CheckBashWithArgs(ctx, c.Call.Detail, c.Call.Args)
	case CheckFileWrite:
		path := c.Call.Detail
		if !filepath.IsAbs(path) {
			path = filepath.Join(Workspace, path)
		}
		checkErr = g.CheckFileWriteWithArgs(ctx, c.Call.Tool, path, c.Call.Args)
	case CheckGeneric:
		checkErr = g.CheckGenericWithArgs(ctx, c.Call.Tool, c.Call.Detail, c.Call.Args)
	case CheckToolCall:
		checkErr = g.CheckToolCallWithArgs(ctx, c.Call.Namespace, c.Call.Tool, c.Call.GateDetail(), c.Call.Args)
	}
	out := Outcome{
		ApproverCalls: counted.n,
		Audit:         turn.audit,
		Prompted:      prompter.asked,
		Allowed:       checkErr == nil,
		InputTokens:   turn.in,
		OutputTokens:  turn.out,
		Elapsed:       time.Since(start),
	}
	return out, nil
}
