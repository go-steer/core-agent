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
	"sort"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
)

// GrantStateKeyPrefix namespaces the durable record of one minted
// change-set grant. One key per authorized call.
const GrantStateKeyPrefix = "mast_change_grant_"

// GrantStateKey is the state key holding the grant for one call
// signature.
func GrantStateKey(signature string) string {
	sum := sha256.Sum256([]byte(signature))
	return GrantStateKeyPrefix + hex.EncodeToString(sum[:])[:32]
}

// Grant is the durable record of one authorized call.
type Grant struct {
	Signature    string                `json:"signature"`
	Tool         string                `json:"tool"`
	Arguments    map[string]any        `json:"arguments"`
	Origin       string                `json:"origin"`
	Approver     string                `json:"approver,omitempty"`
	Note         string                `json:"note,omitempty"`
	MintedAt     time.Time             `json:"minted_at"`
	ExpiresAt    time.Time             `json:"expires_at"`
	Precondition *PreconditionSnapshot `json:"precondition,omitempty"`
	ConsumedBy   string                `json:"consumed_by,omitempty"`
	VoidedBy     string                `json:"voided_by,omitempty"`
}

// Spent reports whether this grant can still authorize a call.
func (g Grant) Spent(functionCallID string) bool {
	if g.VoidedBy != "" {
		return true
	}
	return g.ConsumedBy != "" && g.ConsumedBy != functionCallID
}

// EncodeGrant renders a grant for durable state.
func EncodeGrant(g Grant) (string, error) {
	raw, err := json.Marshal(g)
	if err != nil {
		return "", fmt.Errorf("approval: encoding change-set grant: %w", err)
	}
	return string(raw), nil
}

// DecodeGrant reads a grant back out of durable state.
func DecodeGrant(v any) (Grant, error) {
	var raw []byte
	switch t := v.(type) {
	case nil:
		return Grant{}, fmt.Errorf("approval: no grant record")
	case string:
		raw = []byte(t)
	case []byte:
		raw = t
	default:
		b, err := json.Marshal(v)
		if err != nil {
			return Grant{}, fmt.Errorf("approval: re-marshalling grant record: %w", err)
		}
		raw = b
	}
	var g Grant
	if err := json.Unmarshal(raw, &g); err != nil {
		return Grant{}, fmt.Errorf("approval: grant record is not one: %w", err)
	}
	return g, nil
}

// Precondition is a workload's declaration of what a change assumed
// about the environment, expressed as a read this deployment can make.
type Precondition struct {
	Read     string            `json:"read"`
	Args     map[string]any    `json:"args,omitempty"`
	ArgsFrom map[string]string `json:"args_from,omitempty"`
	Fields   []string          `json:"fields,omitempty"`
}

// PreconditionSnapshot is what the read returned at approval time.
type PreconditionSnapshot struct {
	Read   string            `json:"read"`
	Args   map[string]any    `json:"args,omitempty"`
	Digest string            `json:"digest"`
	Fields map[string]string `json:"fields,omitempty"`
}

// Freshness bounds how long an approval speaks for.
type Freshness struct {
	TTL          time.Duration
	Now          func() time.Time
	Precondition func(toolName string) (*Precondition, error)
	Read         func(ctx agent.Context, toolName string, args map[string]any) (map[string]any, error)
}

// DefaultGrantTTL is how long a change-set grant lives when the bundle
// does not say.
const DefaultGrantTTL = 10 * time.Minute

func (f *Freshness) ttl() time.Duration {
	if f == nil || f.TTL <= 0 {
		return DefaultGrantTTL
	}
	return f.TTL
}

func (f *Freshness) now() time.Time {
	if f != nil && f.Now != nil {
		return f.Now()
	}
	return time.Now()
}

func (f *Freshness) snapshot(ctx agent.Context, ch ProposedChange) (*PreconditionSnapshot, error) {
	if f == nil || f.Precondition == nil {
		return nil, nil
	}
	pre, err := f.Precondition(ch.Tool)
	if err != nil {
		return nil, fmt.Errorf("tool %q: %w", ch.Tool, err)
	}
	if pre == nil {
		return nil, nil
	}
	args, err := pre.readArgs(ch)
	if err != nil {
		return nil, err
	}
	if f.Read == nil {
		return nil, fmt.Errorf("tool %q declares a precondition (read %q) but this deployment cannot run a read on its own behalf", ch.Tool, pre.Read)
	}
	result, err := f.Read(ctx, pre.Read, args)
	if err != nil {
		return nil, fmt.Errorf("precondition read %s failed: %w", CallKey(pre.Read, args), err)
	}
	digest, fields, err := digestResult(result, pre.Fields)
	if err != nil {
		return nil, fmt.Errorf("precondition read %s: %w", CallKey(pre.Read, args), err)
	}
	return &PreconditionSnapshot{Read: pre.Read, Args: args, Digest: digest, Fields: fields}, nil
}

func (f *Freshness) verify(ctx agent.Context, snap *PreconditionSnapshot, toolName string) string {
	if snap == nil {
		return ""
	}
	if f == nil || f.Read == nil || f.Precondition == nil {
		return fmt.Sprintf("this deployment can no longer evaluate %s's precondition (read %q), so the approval is not being relied on", toolName, snap.Read)
	}
	pre, err := f.Precondition(toolName)
	if err != nil || pre == nil {
		return fmt.Sprintf("%s's precondition declaration is no longer readable, so the approval is not being relied on", toolName)
	}
	result, err := f.Read(ctx, snap.Read, snap.Args)
	if err != nil {
		return fmt.Sprintf("the precondition read %s failed (%v), so mast cannot confirm the cluster still matches what was approved", CallKey(snap.Read, snap.Args), err)
	}
	digest, fields, err := digestResult(result, pre.Fields)
	if err != nil {
		return fmt.Sprintf("the precondition read %s returned something mast could not compare (%v)", CallKey(snap.Read, snap.Args), err)
	}
	if digest == snap.Digest {
		return ""
	}
	var moved []string
	for _, name := range sortedFieldNames(snap.Fields, fields) {
		was, now := snap.Fields[name], fields[name]
		if was != now {
			moved = append(moved, fmt.Sprintf("%s was %s at approval and is %s now", name, orNone(was), orNone(now)))
		}
	}
	if len(moved) > 0 {
		return fmt.Sprintf("the cluster moved since this was approved: %s (precondition read %s)",
			strings.Join(moved, "; "), CallKey(snap.Read, snap.Args))
	}
	return fmt.Sprintf("the precondition read %s no longer returns what it returned at approval time", CallKey(snap.Read, snap.Args))
}

func orNone(s string) string {
	if s == "" {
		return "absent"
	}
	return s
}

func sortedFieldNames(a, b map[string]string) []string {
	seen := map[string]bool{}
	for k := range a {
		seen[k] = true
	}
	for k := range b {
		seen[k] = true
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func (p Precondition) readArgs(ch ProposedChange) (map[string]any, error) {
	if strings.TrimSpace(p.Read) == "" {
		return nil, fmt.Errorf("tool %q declares a precondition with no read tool", ch.Tool)
	}
	args := make(map[string]any, len(p.Args)+len(p.ArgsFrom))
	for k, v := range p.Args {
		args[k] = v
	}
	for readArg, changeArg := range p.ArgsFrom {
		v, ok := ch.Arguments[changeArg]
		if !ok {
			return nil, fmt.Errorf("tool %q's precondition takes %s from the call's %q argument, which this call does not carry", ch.Tool, readArg, changeArg)
		}
		args[readArg] = v
	}
	return args, nil
}

func digestResult(result map[string]any, fields []string) (string, map[string]string, error) {
	raw, err := json.Marshal(result)
	if err != nil {
		return "", nil, fmt.Errorf("result is not encodable: %w", err)
	}
	sum := sha256.Sum256(raw)
	digest := hex.EncodeToString(sum[:])[:16]
	if len(fields) == 0 {
		return digest, nil, nil
	}
	out := make(map[string]string, len(fields))
	for _, path := range fields {
		v, ok := lookupPath(result, path)
		if !ok {
			return "", nil, fmt.Errorf("declared field %q is not in the result", path)
		}
		rendered, err := json.Marshal(v)
		if err != nil {
			return "", nil, fmt.Errorf("declared field %q is not encodable: %w", path, err)
		}
		out[path] = string(rendered)
	}
	return digest, out, nil
}

func lookupPath(m map[string]any, path string) (any, bool) {
	cur := any(m)
	for _, seg := range strings.Split(path, ".") {
		obj, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = obj[seg]
		if !ok {
			return nil, false
		}
	}
	return cur, true
}

// Legible reports whether a change set is one an operator can approve
// as a set.
func Legible(changes []ProposedChange) error {
	for i, ch := range changes {
		keys := make([]string, 0, len(ch.Arguments))
		for k := range ch.Arguments {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if elided(ch.Arguments[k]) {
				return fmt.Errorf("%s[%d] calls %s with a %q argument too large to show in the approval question", ChangeSetField, i, ch.Tool, k)
			}
		}
	}
	return nil
}

func elided(v any) bool {
	if s, ok := v.(string); ok {
		return len(s) > maxValueLen
	}
	b, err := json.Marshal(v)
	if err != nil {
		return true
	}
	return len(b) > maxValueLen
}
