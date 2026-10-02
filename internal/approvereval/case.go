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

// Package approvereval evaluates auto mode's approver against a
// labelled corpus (#1175 phase 5, docs/auto-mode-design.md
// "Evaluation"). It is the method of the #652 eval tier — labelled
// cases, a verdict that refuses to count a case that tested nothing,
// and a grader proven able to fail — applied to a verdict classifier
// rather than to an agent run, so it calls the gate directly instead of
// running the binary.
//
// Every case runs through a real permissions.Gate in ModeAuto with the
// approver under test wired in, so production's rules apply on the way
// to and from the model: what is eligible, what never reaches the
// approver, and what an unusable answer becomes. The verdict is read
// from the gate's own audit record.
package approvereval

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/go-steer/core-agent/v2/pkg/tools"
)

// Class is a case's label.
type Class string

const (
	// MustDeny cases are destructive, outward-facing, off-task or
	// injection-bearing calls. Allowing one is the gating failure.
	MustDeny Class = "must_deny"
	// Routine cases are in-task reads, edits and test runs. A deny or an
	// escalation is a false escalate: reported, not gating.
	Routine Class = "routine"
	// MustEscalate cases must reach a person without an approver call:
	// path scope, control plane, subagent and no-task calls. Decided in
	// code, so the corpus checks it without a model as well.
	MustEscalate Class = "must_escalate"
)

// Check names the gate entry point a call goes through, as the tool
// that makes it would.
type Check string

const (
	CheckBash      Check = "bash"       // Gate.CheckBashWithArgs(detail)
	CheckFileWrite Check = "file_write" // Gate.CheckFileWriteWithArgs(tool, detail as path)
	CheckGeneric   Check = "generic"    // Gate.CheckGenericWithArgs(tool, detail)
	// CheckToolCall is a gated toolset call (an MCP or skill tool):
	// Gate.CheckToolCallWithArgs(namespace, tool, detail), with detail
	// built from the arguments by production's tools.SummarizeToolCall
	// — the tool name and the arguments' JSON cut at 200 bytes — never
	// written by hand. It is the one entry point whose detail hides
	// the tail of the arguments.
	CheckToolCall Check = "tool_call"
)

// Call is the call under judgement.
type Call struct {
	Check Check `json:"check"`
	// Namespace is a tool_call's toolset (an MCP server's prefix); the
	// gate's policy and the eligible list match it, not the tool.
	Namespace string          `json:"namespace,omitempty"`
	Tool      string          `json:"tool"`
	Detail    string          `json:"detail,omitempty"`
	Args      json.RawMessage `json:"args"`
}

// GateDetail is the detail the gate sees for the call: Detail as
// written, or for a tool_call the production summary of the arguments.
func (c Call) GateDetail() string {
	if c.Check != CheckToolCall {
		return c.Detail
	}
	var args any
	if len(c.Args) > 0 {
		_ = json.Unmarshal(c.Args, &args)
	}
	return tools.SummarizeToolCall(c.Tool, args)
}

// RecentCall is an earlier call in the turn, as the approver sees it.
type RecentCall struct {
	Tool   string `json:"tool"`
	Detail string `json:"detail"`
}

// Case is one labelled call.
type Case struct {
	ID    string `json:"id"`
	Class Class  `json:"class"`
	// Why says what makes the label right — what the approver must see
	// in the call to get it. Required.
	Why string `json:"why"`
	// Task is the operator's text the approver judges against. "" is a
	// no-task call, which escalates in code.
	Task        string       `json:"task"`
	RecentCalls []RecentCall `json:"recent_calls,omitempty"`
	Call        Call         `json:"call"`
	// Eligible is the recipe's permissions.auto.eligible list for this
	// case. Required: a case that relies on a default tests the default.
	Eligible []string `json:"eligible"`
	// Source, when set, makes the call a background subagent's.
	Source string `json:"source,omitempty"`
}

// Load reads every *.json case in dir, sorted by id. A case file must be
// named after its id, decode strictly, and pass Validate.
func Load(dir string) ([]Case, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, err
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("approvereval: no cases in %s", dir)
	}
	var out []Case
	for _, p := range paths {
		raw, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		var c Case
		dec := json.NewDecoder(bytes.NewReader(raw))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("approvereval: %s: %w", p, err)
		}
		if want := strings.TrimSuffix(filepath.Base(p), ".json"); c.ID != want {
			return nil, fmt.Errorf("approvereval: %s: id %q does not match the file name", p, c.ID)
		}
		if err := c.Validate(); err != nil {
			return nil, fmt.Errorf("approvereval: %s: %w", p, err)
		}
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out, nil
}

// Validate checks a case is well-formed.
func (c Case) Validate() error {
	var errs []error
	switch c.Class {
	case MustDeny, Routine, MustEscalate:
	default:
		errs = append(errs, fmt.Errorf("class %q is not must_deny, routine or must_escalate", c.Class))
	}
	if strings.TrimSpace(c.Why) == "" {
		errs = append(errs, errors.New("why is required"))
	}
	switch c.Call.Check {
	case CheckBash, CheckFileWrite, CheckGeneric:
		if c.Call.Tool == "" || c.Call.Detail == "" {
			errs = append(errs, errors.New("call.tool and call.detail are required"))
		}
	case CheckToolCall:
		if c.Call.Namespace == "" || c.Call.Tool == "" {
			errs = append(errs, errors.New("a tool_call needs call.namespace and call.tool"))
		}
		if c.Call.Detail != "" {
			errs = append(errs, errors.New("a tool_call's detail is built from its args by production's summarizer; do not write one"))
		}
	default:
		errs = append(errs, fmt.Errorf("call.check %q is not bash, file_write, generic or tool_call", c.Call.Check))
	}
	if len(c.Eligible) == 0 {
		errs = append(errs, errors.New("eligible is required: a case that relies on a default tests the default"))
	}
	if c.Class != MustEscalate && strings.TrimSpace(c.Task) == "" {
		errs = append(errs, errors.New("a must_deny or routine case needs a task: with none the gate escalates before the approver is asked"))
	}
	return errors.Join(errs...)
}
