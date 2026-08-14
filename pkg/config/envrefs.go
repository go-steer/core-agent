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

package config

import (
	"reflect"
	"sort"
	"strings"
)

// envRefSuffix is the naming convention that marks a config field as
// holding the NAME of an env var rather than a value: any field whose
// JSON tag ends in "_env" (url_env, token_env, bearer_env) or is exactly
// one of the basic-auth pair. See EnvRefs for why the convention is the
// contract.
const envRefSuffix = "_env"

// extraEnvRefTags are the env-name fields whose JSON tags predate the
// _env-suffix convention and can't be renamed without breaking every
// config in the wild. Keep this list short; new fields should take the
// suffix instead of an entry here.
var extraEnvRefTags = map[string]struct{}{
	"basic_env_user": {},
	"basic_env_pass": {},
}

// EnvRefs returns every env-var NAME the config refers to by name,
// sorted and deduplicated.
//
// The bundle has two ways to consume an env var, and they are not
// interchangeable:
//
//   - ${env:NAME} in instruction files, skills, and mcp.json values —
//     splices the VALUE into text the model reads, at load time.
//   - a *_env config field naming the var — hands the NAME to a
//     component that resolves it late (pkg/tools/alert re-reads url_env
//     on every fire, so rotating a Secret needs no restart, and the
//     token never lands in the in-memory Config that log and status
//     surfaces can echo).
//
// pkg/agentenv only ever saw the first, because config never flows
// through Interpolate. That made .agents/env.yaml's drift report lie in
// both directions: a var declared in the manifest and used only by
// alerts.targets[].url_env was reported "nothing in the bundle
// references it" (observed live 2026-08-14 on the kube-platform-native
// deployment), and a bearer_env naming a var the manifest never
// declared drew no warning at all. Feeding this set into
// Resolver.NoteConfigRefs before ReportDrift closes both.
//
// Discovery is reflective over the JSON tags rather than a hand-written
// field list, for the same reason pkg/tools derives its cross-reference
// catalog from the specs table (#759): a list maintained beside the
// thing it describes drifts the first time someone adds a field and
// doesn't know the list exists. The cost is that the convention IS the
// contract — a future env-name field tagged `json:"webhook_secret"`
// would go unseen. That is a naming review comment, not a silent bug:
// the failure mode is one spurious drift warning, never a boot failure
// or a misresolved value.
//
// A nil *Config returns nil.
func (c *Config) EnvRefs() []string {
	if c == nil {
		return nil
	}
	seen := make(map[string]struct{})
	collectEnvRefs(reflect.ValueOf(c), seen, make(map[uintptr]struct{}), 0)
	if len(seen) == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for name := range seen {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// maxEnvRefDepth bounds the walk. Config is a shallow tree (deepest
// today is attach.listeners[].auth), so this is a guard against a future
// self-referential type turning a boot diagnostic into a hang, not a
// real limit anyone should hit.
const maxEnvRefDepth = 12

// collectEnvRefs walks v, adding the value of every string field whose
// JSON tag marks it an env-var name. visited breaks pointer cycles.
func collectEnvRefs(v reflect.Value, seen map[string]struct{}, visited map[uintptr]struct{}, depth int) {
	if depth > maxEnvRefDepth || !v.IsValid() {
		return
	}
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return
		}
		if v.Kind() == reflect.Pointer {
			if _, dup := visited[v.Pointer()]; dup {
				return
			}
			visited[v.Pointer()] = struct{}{}
		}
		collectEnvRefs(v.Elem(), seen, visited, depth+1)
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			collectEnvRefs(v.Index(i), seen, visited, depth+1)
		}
	case reflect.Map:
		for _, k := range v.MapKeys() {
			collectEnvRefs(v.MapIndex(k), seen, visited, depth+1)
		}
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if !f.IsExported() {
				continue
			}
			fv := v.Field(i)
			if fv.Kind() == reflect.String {
				if name := strings.TrimSpace(fv.String()); name != "" && isEnvRefTag(jsonTagName(f)) {
					seen[name] = struct{}{}
				}
				continue
			}
			collectEnvRefs(fv, seen, visited, depth+1)
		}
	}
}

// isEnvRefTag reports whether a JSON tag names an env-var-name field.
func isEnvRefTag(tag string) bool {
	if tag == "" {
		return false
	}
	if _, ok := extraEnvRefTags[tag]; ok {
		return true
	}
	return strings.HasSuffix(tag, envRefSuffix)
}

// jsonTagName returns the field's JSON name, or "" when the field is
// explicitly skipped with `json:"-"`. Falls back to the Go field name
// when there is no tag, matching encoding/json.
func jsonTagName(f reflect.StructField) string {
	tag, ok := f.Tag.Lookup("json")
	if !ok {
		return f.Name
	}
	name, _, _ := strings.Cut(tag, ",")
	if name == "-" {
		return ""
	}
	if name == "" {
		return f.Name
	}
	return name
}
