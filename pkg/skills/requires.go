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

package skills

import (
	"bytes"
	"fmt"
	"io/fs"
	"os/exec"
	"path"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/go-steer/core-agent/v2/pkg/permissions"
)

// requiresKey is the SKILL.md frontmatter key a bundle uses to declare
// what the runtime must actually HAVE for the skill's instructions to
// mean anything (#962).
//
//	---
//	name: gke-workload-troubleshooting
//	requires: [shell, kubectl, gcloud]
//	---
//
// It is a capability claim, not a permission: `allowed-tools` says what
// a skill MAY use, `requires` says what must exist before the body is
// worth serving at all. They are deliberately not merged.
//
// The published image is distroless — no shell, no kubectl, no gcloud.
// Without this key a skill naming them loads happily and then instructs
// the model to do something the runtime cannot do; #644 and #674 were
// that bug twice, and the #704 case study measured 183 instances of it
// in one ported recipe. A dropped skill is a startup line an operator
// can act on; a loaded-but-unrunnable skill is a turn the model spends
// discovering the same thing the hard way.
const requiresKey = "requires"

// malformedRequires is the reason reported for a `requires:` key that is
// present but not a string or a list of strings.
//
// A bundle whose requirements cannot be READ is unsatisfiable by
// inspection, so it is dropped like any other — the alternative (ignore
// the key, load the skill) restores the exact silent failure the key
// exists to end, and does it to the author who was trying to use it.
const malformedRequires = `malformed "requires:" — must be a string or a list of strings`

// Unsatisfied names a skill that was discovered on disk but withheld
// from the toolset, and says why in one line fit for stderr.
type Unsatisfied struct {
	// Skill is the skill's name, which is also its directory name.
	Skill string
	// Reason is a complete, human-readable clause: "unmet \"requires:\"
	// — kubectl (not on PATH)". Hosts print it verbatim.
	Reason string
}

// String renders the drop as one operator-facing line.
func (u Unsatisfied) String() string { return fmt.Sprintf("%s: %s", u.Skill, u.Reason) }

// requireDecl is one skill's parsed `requires:` key. A skill with no
// such key gets no requireDecl at all, so the zero value never means
// "no requirements" — absence does.
type requireDecl struct {
	tokens []string
	// err is non-empty when the key was present but unreadable, which is
	// itself a drop reason.
	err string
}

// capabilities answers the two questions the requirement grammar asks.
type capabilities struct {
	// shellTool reports whether THIS build registered the `bash` tool.
	// Deliberately not "is there a /bin/bash": the failure #644 and #674
	// hit was a build that disabled the bash tool, on machines whose
	// shell was right there. What the model can invoke is the question.
	shellTool func() bool
	// lookPath resolves a bare requirement token to an executable.
	lookPath func(string) (string, error)
}

// capabilitiesFor builds the resolver for a load.
//
// The shell answer comes off the gate by default, because the gate is
// already told what tools.Build registered (SetRegisteredTools) and is
// already handed to every LoadAll call — so skill reloads, declarative
// subagent roots and multi-session hosts all get the right answer with
// no extra wiring. The one caller that cannot is the daemon's first
// load, which runs BEFORE tools.Build; it passes WithShellTool.
//
// A gate that was never told falls back to "assume registered", matching
// Gate.HasTool. That is fail-open, and deliberately so: refusing to load
// a skill on the strength of a question nobody answered would hide
// working skills from embedders who never opted in.
func capabilitiesFor(lo loadOptions, gate *permissions.Gate) capabilities {
	c := capabilities{lookPath: lo.lookPath}
	if c.lookPath == nil {
		c.lookPath = exec.LookPath
	}
	if lo.shellTool != nil {
		registered := *lo.shellTool
		c.shellTool = func() bool { return registered }
	} else {
		c.shellTool = func() bool { return gate.HasTool("bash") }
	}
	return c
}

// evaluate returns "" when every requirement holds, or a one-line reason
// the skill cannot be served here.
func (c capabilities) evaluate(d requireDecl) string {
	if d.err != "" {
		return d.err
	}
	var missing []string
	for _, tok := range d.tokens {
		switch tok {
		// `bash` is accepted alongside `shell` because that is what the
		// tool is called; both mean the same check, and an author who
		// writes either gets the answer they meant.
		case "shell", "bash":
			if !c.shellTool() {
				missing = append(missing, tok+" (the bash tool is not registered in this build)")
			}
		default:
			if _, err := c.lookPath(tok); err != nil {
				missing = append(missing, tok+" (not on PATH)")
			}
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return `unmet "requires:" — ` + strings.Join(missing, "; ")
}

// scanRequires reads the `requires:` key off each skill's raw SKILL.md.
//
// It reads through the COMPOSED overlay rather than the sanitizing
// wrapper the toolset sees, and that is load-bearing: sanitizeFrontmatter
// filters frontmatter down to the fields ADK's parser accepts, so by the
// time a SKILL.md reaches the toolset the `requires:` key is gone. The
// overlay is still the right layer to read from — it resolves
// precedence, so a project skill shadowing a user-global one of the same
// name contributes its own requirements and not the shadowed bundle's.
//
// Keyed by directory name, which ADK's filesystem source requires to
// equal the frontmatter `name` (readSkill rejects a mismatch outright),
// so the key matches skill.Frontmatter.Name for every skill that loads.
//
// Anything unreadable — no SKILL.md, no frontmatter fence, invalid YAML
// — yields no entry. Those are ListFrontmatters' failures to report, and
// reporting them twice, in different words, from a key that may not even
// be present would be worse than silence here.
func scanRequires(fsys fs.FS) map[string]requireDecl {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil
	}
	out := make(map[string]requireDecl)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		data, err := fs.ReadFile(fsys, path.Join(e.Name(), "SKILL.md"))
		if err != nil {
			continue
		}
		if decl, ok := parseRequires(data); ok {
			out[e.Name()] = decl
		}
	}
	return out
}

// parseRequires extracts the `requires:` key from raw SKILL.md bytes.
// The bool reports whether the key was present at all.
func parseRequires(data []byte) (requireDecl, bool) {
	block := frontmatterYAML(data)
	if block == nil {
		return requireDecl{}, false
	}
	var raw map[string]any
	if err := yaml.Unmarshal(block, &raw); err != nil {
		return requireDecl{}, false
	}
	v, ok := raw[requiresKey]
	if !ok {
		return requireDecl{}, false
	}
	switch t := v.(type) {
	case string:
		tok := strings.TrimSpace(t)
		if tok == "" {
			return requireDecl{err: malformedRequires}, true
		}
		return requireDecl{tokens: []string{tok}}, true
	case []any:
		// `requires: []` is an author saying "nothing" out loud. Honour
		// it as no requirements rather than as a malformed key.
		if len(t) == 0 {
			return requireDecl{}, false
		}
		tokens := make([]string, 0, len(t))
		for _, elem := range t {
			s, isStr := elem.(string)
			if !isStr || strings.TrimSpace(s) == "" {
				return requireDecl{err: malformedRequires}, true
			}
			tokens = append(tokens, strings.TrimSpace(s))
		}
		return requireDecl{tokens: tokens}, true
	default:
		return requireDecl{err: malformedRequires}, true
	}
}

// frontmatterYAML returns the raw YAML between a SKILL.md's leading
// `---` fences, or nil when there is no frontmatter block. Shared with
// sanitizeFrontmatter so the two passes always agree on where the
// frontmatter ends — a `requires:` key read out of a block the sanitizer
// does not consider frontmatter would drop skills for text in the body.
func frontmatterYAML(data []byte) []byte {
	if !bytes.HasPrefix(data, []byte("---\n")) && !bytes.HasPrefix(data, []byte("---\r\n")) {
		return nil
	}
	parts := bytes.SplitN(data, []byte("---"), 3)
	if len(parts) < 3 {
		return nil
	}
	return parts[1]
}

// partitionByRequirements splits the discovered skill names into the set
// the runtime can honestly serve and the drops, sorted by skill name so
// startup output is stable across runs.
func partitionByRequirements(names []string, decls map[string]requireDecl, caps capabilities) (keep map[string]bool, dropped []Unsatisfied) {
	keep = make(map[string]bool, len(names))
	for _, name := range names {
		decl, declared := decls[name]
		if !declared {
			keep[name] = true
			continue
		}
		if reason := caps.evaluate(decl); reason != "" {
			dropped = append(dropped, Unsatisfied{Skill: name, Reason: reason})
			continue
		}
		keep[name] = true
	}
	sort.Slice(dropped, func(i, j int) bool { return dropped[i].Skill < dropped[j].Skill })
	return keep, dropped
}
