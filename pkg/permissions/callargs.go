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

// The WithArgs siblings: the Check* entry points for a call site that
// holds the call's full arguments (#1175 phase 2).
//
// ModeAuto's approver judges only a call the gate holds whole
// (decision 3): the key a Check* method takes is a summary — cut at 200
// bytes for namespaced tools, a bare target name for alert and
// call_peer, and nothing of write_file's content at all. Each sibling
// is its plain counterpart plus the arguments, so outside ModeAuto the
// two behave identically, and a call site that still uses the plain
// form fails closed: its calls escalate to a person.
//
// Siblings rather than a new parameter, for the reason
// CheckReadOnlyToolCall gives: pkg/permissions is inside the stability
// promise, and a host embedding the gate keeps compiling.

package permissions

import (
	"bytes"
	"context"
	"encoding/json"
)

// CheckBashWithArgs is CheckBash for a call site that holds the call's
// full arguments. See CallArgs for what args may be.
func (g *Gate) CheckBashWithArgs(ctx context.Context, command string, args any) error {
	return g.checkBash(ctx, command, deferCallArgs(args))
}

// CheckFileWriteWithArgs is CheckFileWrite for a call site that holds
// the call's full arguments — for write_file, the content as well as
// the path.
func (g *Gate) CheckFileWriteWithArgs(ctx context.Context, toolName, path string, args any) error {
	return g.checkFileWrite(ctx, toolName, path, deferCallArgs(args))
}

// CheckGenericWithArgs is CheckGeneric for a call site that holds the
// call's full arguments.
func (g *Gate) CheckGenericWithArgs(ctx context.Context, toolName, key string, args any) error {
	return g.checkGeneric(ctx, toolName, key, deferCallArgs(args))
}

// CheckToolCallWithArgs is CheckToolCall for a call site that holds the
// call's full arguments.
func (g *Gate) CheckToolCallWithArgs(ctx context.Context, namespace, tool, key string, args any) error {
	return g.checkToolCall(ctx, namespace, tool, key, false, deferCallArgs(args))
}

// CheckReadOnlyToolCallWithArgs is CheckReadOnlyToolCall for a call site
// that holds the call's full arguments.
func (g *Gate) CheckReadOnlyToolCallWithArgs(ctx context.Context, namespace, tool, key string, args any) error {
	return g.checkToolCall(ctx, namespace, tool, key, true, deferCallArgs(args))
}

// lazyArgs yields a call's canonical arguments. The gate holds them
// this way because only a prompt reads them: a call that a policy
// entry, a session grant or yolo settles never pays for re-encoding a
// write_file's whole content, which costs several copies of it. A nil
// lazyArgs is a call site that passed none.
type lazyArgs func() json.RawMessage

func (l lazyArgs) get() json.RawMessage {
	if l == nil {
		return nil
	}
	return l()
}

// deferCallArgs is CallArgs, run when the gate asks for the result.
// The tool's input is not mutated between the check and the prompt:
// both happen inside the one Check* call.
func deferCallArgs(args any) lazyArgs {
	return func() json.RawMessage { return CallArgs(args) }
}

// jsonSpelling is s as the canonical encoder writes it inside a JSON
// string. CallArgs resolves every optional escape, but the encoder
// still escapes a backslash, a quote, control characters and
// U+2028/U+2029, so a never-list mention holding one of those can only
// match canonical Args in this form.
func jsonSpelling(s string) string {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(s); err != nil {
		return s
	}
	b := bytes.TrimRight(out.Bytes(), "\n")
	return string(b[1 : len(b)-1])
}

// CallArgs renders a call's arguments in the one form the gate
// compares: args is the tool's decoded input — its typed args struct,
// or the map a toolset received — and it is marshalled here, by the
// gate, so no call site can hand the approver the model's raw bytes.
//
// That matters to the never-list (decision 4), which is a substring
// match: raw JSON can spell `.agents/config.json` as
// `.agents\/config.json` or with \u escapes, and only a canonical
// encoding makes the match mean anything. A json.RawMessage or []byte
// is decoded and re-encoded for the same reason.
//
// Returns nil — so the call escalates — when args is nil, encodes to
// JSON null, or cannot be encoded at all.
func CallArgs(args any) json.RawMessage {
	if args == nil {
		return nil
	}
	switch raw := args.(type) {
	case json.RawMessage:
		return canonicalJSON(raw)
	case []byte:
		return canonicalJSON(raw)
	}
	b, err := json.Marshal(args)
	if err != nil {
		return nil
	}
	// A struct's own MarshalJSON can emit escapes json.Marshal would
	// not, so re-encode the result too.
	return canonicalJSON(b)
}

// canonicalJSON decodes b and encodes it again, which resolves every
// string escape to the encoder's single spelling. Numbers stay
// json.Number so a large integer is not rounded through float64.
func canonicalJSON(b []byte) json.RawMessage {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil || v == nil {
		return nil
	}
	if dec.More() {
		return nil
	}
	// No HTML escaping: json.Marshal would spell < > & as \u003c and so
	// on, and a never-list entry containing one would then never match.
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil
	}
	return bytes.TrimRight(out.Bytes(), "\n")
}
