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

package approval

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
)

// CaptureStateKeyPrefix namespaces the durable prior-state record of one
// mutating call.
const CaptureStateKeyPrefix = "mast_effect_capture_"

// CaptureStateKey is the state key holding the capture for one call.
func CaptureStateKey(functionCallID string) string {
	return CaptureStateKeyPrefix + functionCallID
}

// Capture is a workload's declaration of how to record what a mutating
// call is about to change.
type Capture struct {
	Read     string            `json:"read"`
	Args     map[string]any    `json:"args,omitempty"`
	ArgsFrom map[string]string `json:"args_from,omitempty"`
	Fields   []string          `json:"fields,omitempty"`
	Revert   *Revert           `json:"revert,omitempty"`
}

// Revert declares the call that undoes a change, in terms of the change
// and the state captured before it.
type Revert struct {
	Call            string            `json:"call"`
	Args            map[string]any    `json:"args,omitempty"`
	ArgsFromChange  map[string]string `json:"args_from_change,omitempty"`
	ArgsFromCapture map[string]string `json:"args_from_capture,omitempty"`
}

// CaptureRecord is the durable answer to "how do I undo this": one
// mutating call, the state it was about to overwrite, and the call that
// puts that state back.
type CaptureRecord struct {
	Tool           string          `json:"tool"`
	Key            string          `json:"key"`
	Arguments      map[string]any  `json:"arguments"`
	CapturedAt     time.Time       `json:"captured_at"`
	Read           string          `json:"read"`
	ReadArgs       map[string]any  `json:"read_args,omitempty"`
	Prior          map[string]any  `json:"prior"`
	PriorFields    []string        `json:"prior_fields,omitempty"`
	Digest         string          `json:"digest"`
	Revert         *ProposedChange `json:"revert,omitempty"`
	Workload       string          `json:"workload,omitempty"`
	Specialist     string          `json:"specialist,omitempty"`
	Session        string          `json:"session,omitempty"`
	Invocation     string          `json:"invocation,omitempty"`
	FunctionCallID string          `json:"function_call_id,omitempty"`
}

// Undoable reports whether this record names a call that would restore
// the captured state.
func (r CaptureRecord) Undoable() bool { return r.Revert != nil }

// EncodeCapture renders a capture for durable state.
func EncodeCapture(r CaptureRecord) (string, error) {
	raw, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("approval: encoding prior-state capture: %w", err)
	}
	return string(raw), nil
}

// DecodeCapture parses a record written under a CaptureStateKey.
func DecodeCapture(v any) (CaptureRecord, error) {
	var raw []byte
	switch t := v.(type) {
	case nil:
		return CaptureRecord{}, fmt.Errorf("approval: no capture record")
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return CaptureRecord{}, fmt.Errorf("approval: re-marshalling capture record: %w", err)
		}
		raw = b
	}
	var r CaptureRecord
	if err := json.Unmarshal(raw, &r); err != nil {
		return CaptureRecord{}, fmt.Errorf("approval: capture record is not one: %w", err)
	}
	return r, nil
}

// CaptureRules is the runtime half of prior-state capture: what each
// tool declares, and the two things needed to honor a declaration.
type CaptureRules struct {
	For    func(toolName string) (*Capture, error)
	Read   func(ctx agent.Context, toolName string, args map[string]any) (map[string]any, error)
	Schema func(toolName string) (*jsonschema.Schema, error)
	Now    func() time.Time
}

func (c *CaptureRules) now() time.Time {
	if c != nil && c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *CaptureRules) declared(toolName string) (*Capture, error) {
	if c == nil || c.For == nil {
		return nil, nil
	}
	return c.For(toolName)
}

func (c *CaptureRules) take(ctx agent.Context, toolName, key string, args map[string]any, decl *Capture) (*CaptureRecord, error) {
	if decl == nil {
		return nil, nil
	}
	readArgs, err := decl.readArgs(toolName, args)
	if err != nil {
		return nil, err
	}
	if c.Read == nil {
		return nil, fmt.Errorf("tool %q declares a prior-state capture (read %q) but this deployment cannot run a read on its own behalf", toolName, decl.Read)
	}
	result, err := c.Read(ctx, decl.Read, readArgs)
	if err != nil {
		return nil, fmt.Errorf("capture read %s failed: %w", CallKey(decl.Read, readArgs), err)
	}
	if result == nil {
		return nil, fmt.Errorf("capture read %s returned no result, so there is nothing recorded to go back to", CallKey(decl.Read, readArgs))
	}
	prior, err := decl.prior(result)
	if err != nil {
		return nil, fmt.Errorf("capture read %s: %w", CallKey(decl.Read, readArgs), err)
	}
	rec := &CaptureRecord{
		Tool:        toolName,
		Key:         key,
		Arguments:   copyArgs(args),
		CapturedAt:  c.now().UTC(),
		Read:        decl.Read,
		ReadArgs:    readArgs,
		Prior:       prior,
		PriorFields: append([]string(nil), decl.Fields...),
		Digest:      resultDigest(result),
	}
	if ctx != nil {
		rec.Specialist = ctx.AgentName()
		rec.Session = ctx.SessionID()
		rec.Invocation = ctx.InvocationID()
		rec.FunctionCallID = ctx.FunctionCallID()
	}
	if decl.Revert != nil {
		ch, err := c.revert(toolName, args, result, decl.Revert)
		if err != nil {
			return nil, err
		}
		rec.Revert = ch
	}
	return rec, nil
}

func (c *CaptureRules) revert(toolName string, args, result map[string]any, r *Revert) (*ProposedChange, error) {
	call := strings.TrimSpace(r.Call)
	if call == "" {
		return nil, fmt.Errorf("tool %q declares a revert with no call", toolName)
	}
	out := make(map[string]any, len(r.Args)+len(r.ArgsFromChange)+len(r.ArgsFromCapture))
	for k, v := range r.Args {
		out[k] = v
	}
	for revertArg, changeArg := range r.ArgsFromChange {
		v, ok := args[changeArg]
		if !ok {
			return nil, fmt.Errorf("tool %q's revert takes %s from the call's %q argument, which this call does not carry", toolName, revertArg, changeArg)
		}
		out[revertArg] = v
	}
	for revertArg, path := range r.ArgsFromCapture {
		v, ok := lookupPath(result, path)
		if !ok {
			return nil, fmt.Errorf("tool %q's revert takes %s from the captured %q, which the capture read did not return", toolName, revertArg, path)
		}
		out[revertArg] = v
	}
	if c.Schema == nil {
		return nil, fmt.Errorf("tool %q declares a revert but this deployment cannot look up %q's arguments, so mast cannot tell whether the revert it built is callable", toolName, call)
	}
	schema, err := c.Schema(call)
	if err != nil {
		return nil, fmt.Errorf("tool %q's revert calls %q: %w", toolName, call, err)
	}
	norm, err := NormalizeArgs(call, schema, out)
	if err != nil {
		return nil, fmt.Errorf("tool %q's revert would call %s, which %q will not accept: %w", toolName, CallKey(call, out), call, err)
	}
	return &ProposedChange{Tool: call, Arguments: norm}, nil
}

func (c Capture) readArgs(toolName string, args map[string]any) (map[string]any, error) {
	if strings.TrimSpace(c.Read) == "" {
		return nil, fmt.Errorf("tool %q declares a prior-state capture with no read tool", toolName)
	}
	out := make(map[string]any, len(c.Args)+len(c.ArgsFrom))
	for k, v := range c.Args {
		out[k] = v
	}
	for readArg, changeArg := range c.ArgsFrom {
		v, ok := args[changeArg]
		if !ok {
			return nil, fmt.Errorf("tool %q's capture takes %s from the call's %q argument, which this call does not carry", toolName, readArg, changeArg)
		}
		out[readArg] = v
	}
	return out, nil
}

func (c Capture) prior(result map[string]any) (map[string]any, error) {
	if len(c.Fields) == 0 {
		return result, nil
	}
	out := make(map[string]any, len(c.Fields))
	for _, path := range c.Fields {
		v, ok := lookupPath(result, path)
		if !ok {
			return nil, fmt.Errorf("declared field %q is not in the result, so the capture would record nothing where it promised the old value", path)
		}
		out[path] = v
	}
	return out, nil
}

func resultDigest(result map[string]any) string {
	raw, err := json.Marshal(result)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

// DescribeCapture renders a capture record as the two lines an operator
// needs: what was overwritten, and the call that puts it back.
func DescribeCapture(r CaptureRecord) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s overwrote state read by %s", r.Key, CallKey(r.Read, r.ReadArgs))
	if len(r.PriorFields) > 0 {
		fmt.Fprintf(&b, " (%s)", strings.Join(r.PriorFields, ", "))
	}
	if r.Revert == nil {
		b.WriteString("; this workload declares no call that puts it back")
		return b.String()
	}
	fmt.Fprintf(&b, "; undo with %s", CallKey(r.Revert.Tool, r.Revert.Arguments))
	return b.String()
}
