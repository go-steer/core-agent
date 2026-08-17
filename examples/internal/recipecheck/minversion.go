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

// This file is the version half of the package (#680): which daemon
// release a recipe's config actually needs.
//
// The tool-reachability check in recipecheck.go answers "can the agent
// reach what the content names" against the CURRENT source tree. That is
// the right question for a developer and the wrong one for an operator,
// who runs a pinned image. pkg/config does not set
// DisallowUnknownFields, so a 2.8.0 binary handed this repo's
// gke-troubleshoot config boots cleanly, silently drops `alerts` and
// `tools.wait_and_verify`, registers neither tool, and then hands the
// model a skill that instructs it to call both. Structurally that is
// #644 one layer down: the config states a property, the deployed
// runtime does not have it, and nothing says so.
//
// The three pieces here:
//
//   - GatedFeatures maps a config path to the first release that
//     understands it.
//   - RequiredVersion reads a recipe's config and returns the highest
//     such release it depends on.
//   - ReleasedVersions reads CHANGELOG.md for the set of versions that
//     exist at all, so a pin can be checked for existence and not just
//     for order. kube-platform-agent pinned "2.9.0", which has never
//     been released — an ordering check alone waves that through.
//
// imagepin.go compares the answer against what the recipe's overlays
// actually pin.
package recipecheck

import (
	"fmt"
	"os"
	"reflect"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/mod/semver"

	"github.com/go-steer/core-agent/v2/pkg/config"
)

// --- versions ---

// Version is one of this repo's release tags, validated to a full
// MAJOR.MINOR.PATCH[-prerelease] shape and ordered by
// golang.org/x/mod/semver.
//
// The ordering is delegated rather than hand-rolled. golang.org/x/mod is
// already in this module's graph (golang.org/x/text depends on it), so
// naming it directly costs one go.mod line at the version the build
// already selected, two go.sum lines, and no new modules — measurably
// cheaper than owning a semver §11 comparator whose prerelease rules
// (numeric identifiers compare numerically, numeric sorts below
// alphanumeric, a longer identifier list wins a tie) are easy to get
// subtly wrong and hard to notice when you do.
//
// The shape check stays local because semver.IsValid is deliberately
// lax about it: IsValid("v2.9") is true, and a `newTag: "2.9"` that
// quietly parsed as a version would be exactly the kind of quiet this
// check exists to remove.
type Version struct {
	// canonical is the x/mod spelling: a leading "v", which is also this
	// repo's git-tag spelling. The empty string is the zero value and
	// sorts below every real release (semver.Compare treats an invalid
	// version as less than a valid one), which is what makes
	// Requirement.Min usable before any feature has raised it.
	canonical string
}

// versionRe accepts both spellings this repo uses: the git tag ("v2.8.0")
// and the GHCR tag, which docker/metadata-action strips the "v" from
// ("2.8.0"). Build metadata is accepted and ignored, per semver §10.
var versionRe = regexp.MustCompile(`^v?(\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?)(?:\+[0-9A-Za-z.-]+)?$`)

// ParseVersion parses a release tag. A floating tag ("main",
// "main-1a2b3c4", "latest") is not a version and returns an error —
// callers are expected to treat that as a finding, not as a pass.
func ParseVersion(s string) (Version, error) {
	m := versionRe.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return Version{}, fmt.Errorf("not a semantic version: %q", s)
	}
	canonical := "v" + m[1]
	if !semver.IsValid(canonical) {
		// versionRe is the stricter of the two, so this is unreachable
		// short of a regex edit. Kept because "unreachable" is a claim
		// with a shelf life.
		return Version{}, fmt.Errorf("not a semantic version: %q", s)
	}
	return Version{canonical: canonical}, nil
}

// String renders the GHCR spelling — no leading "v" — because that is
// what an image pin has to say.
func (v Version) String() string { return strings.TrimPrefix(v.canonical, "v") }

// IsZero reports whether v is the unset version.
func (v Version) IsZero() bool { return v.canonical == "" }

// Compare returns -1, 0 or +1 as v sorts before, equal to, or after o.
func (v Version) Compare(o Version) int { return semver.Compare(v.canonical, o.canonical) }

// --- version-gated config ---

// GatedFeature is one config path a daemon older than Min does not know
// about. Because pkg/config does not set DisallowUnknownFields, "does
// not know about" means "drops without a word".
type GatedFeature struct {
	// Path is a config JSON path in the grammar ConfigSurface writes:
	// struct fields joined with ".", "[]" for a slice of structs, "{}"
	// for a map of structs. It must resolve against the current
	// config.Config — RequiredVersion errors if it does not, so a
	// renamed field turns into a red build instead of a dead table row.
	Path string
	// Min is the first RELEASE that understands Path, spelled as the
	// GHCR tag (no leading "v"). It is the first release, not the first
	// GA: 2.9.0-dev.1 ships the v2.9 config surface and a recipe pinned
	// to it is correct, so requiring "2.9.0" here would be a lie in the
	// strict direction.
	Min string
	// Why states what an older daemon does instead, in operator terms.
	Why string
}

// GatedFeatures is the table. It is deliberately NOT the whole config
// surface.
//
// Strictly, every field is version-gated: an older daemon drops any key
// it does not know. What makes a drop worth a red build is what the
// config was ASSERTING — this table covers the paths that (a) register a
// tool, (b) change the agent topology or the content the agent loads, or
// (c) assert a safety property. Those are the drops that leave the model
// holding a promise the runtime cannot keep, which is the #644 shape.
// `agent.max_steps` silently reverting to its default is a degradation
// an operator can see in the logs; `alerts` silently vanishing is a
// pager that never fires.
//
// Entries at or below the current major's first release (2.0.0) are
// omitted: nothing can pin below v2 and still be this daemon.
//
// Adding an entry: find the first release containing the field with
//
//	sha=$(git log --reverse --format=%H -S'json:"<field>' -- pkg/config | head -1)
//	git tag --contains "$sha" --sort=version:refname | head -1
//
// Forgetting to add one is caught from the other side —
// TestConfigSurfaceIsAccountedFor fails the build when pkg/config grows
// a path this package has never seen, which forces the question.
var GatedFeatures = []GatedFeature{
	{
		Path: "alerts.targets",
		Min:  "2.9.0-dev.1",
		Why: "the `alert` tool registers only when a target is configured (#607). " +
			"An older daemon drops the whole `alerts` block, never registers the tool, " +
			"and skill content that calls alert() is naming a tool that is not in the catalog",
	},
	{
		Path: "tools.wait_and_verify",
		Min:  "2.9.0-dev.1",
		Why: "the `wait_and_verify` tool and its poll_allow assertion list (#672). " +
			"An older daemon has no such tool, so a skill that treats `verified: true` as the " +
			"only grounds for RESOLVED can never reach RESOLVED",
	},
	{
		Path: "tools.call_peer",
		Min:  "2.9.0-dev.1",
		Why:  "the `call_peer` tool registers off this block; an older daemon drops it and never registers the tool",
	},
	{
		Path: "subagents",
		Min:  "2.9.0-dev.1",
		Why: "declarative subagents and the `spawn_agent` tool they register (#602). " +
			"An older daemon drops the roster and runs as a single agent, so every delegation " +
			"instruction in the content is dead",
	},
	{
		Path: "subagents[].root",
		Min:  "2.9.0-dev.1",
		Why: "a per-subagent content root — its own AGENTS.md, skills/ and mcp.json (#619). " +
			"An older daemon drops it and the subagent boots with no instructions and no skills",
	},
	{
		Path: "content_roots",
		Min:  "2.9.0-dev.1",
		Why: "extra instruction/skill trees loaded from outside the agents dir (#610). " +
			"An older daemon loads only the agents dir, so most of the recipe's content never reaches the model",
	},
	{
		Path: "permissions.plan_mode",
		Min:  "2.9.0-dev.1",
		Why:  "plan_mode selects whether `record_plan` is registered at all; an older daemon ignores the setting",
	},
	{
		Path: "safety.watchdog",
		Min:  "2.9.0-dev.1",
		Why:  "the runaway-loop watchdog's enforce/warn selection (#623); an older daemon runs without the backstop the config asked for",
	},
	{
		Path: "safety.bash_search_gate",
		Min:  "2.9.0-dev.1",
		Why:  "the bash search gate; an older daemon ignores it and leaves the gate off",
	},
	{
		Path: "agent.auto_continue",
		Min:  "2.8.0",
		Why:  "auto-continue of a restart-interrupted turn (#559); an older daemon ignores the block, including an explicit opt-out",
	},
	{
		Path: "attach.multi_session",
		Min:  "2.4.0",
		Why:  "the multi-session attach substrate; an older daemon serves a single session and ignores the auth table",
	},
	{
		Path: "agent.max_turn_cost_usd",
		Min:  "2.4.0",
		Why:  "the per-turn spend cap; an older daemon runs uncapped",
	},
	{
		Path: "agent.max_session_cost_usd",
		Min:  "2.4.0",
		Why:  "the per-session spend cap; an older daemon runs uncapped",
	},
}

// Reason is one version-gated feature a config actually uses.
type Reason struct {
	Path string
	Min  Version
	Why  string
}

func (r Reason) String() string {
	return fmt.Sprintf("%s needs ≥ %s — %s", r.Path, r.Min, r.Why)
}

// Requirement is the floor a recipe's config puts under the daemon image
// it is deployed with.
type Requirement struct {
	// Min is the highest GatedFeature.Min among the features in use.
	Min Version
	// Reasons are the features in use, in table order. Empty means the
	// config uses nothing gated.
	Reasons []Reason
}

// Empty reports whether the config asks for nothing beyond the baseline.
func (r Requirement) Empty() bool { return len(r.Reasons) == 0 }

// Unmet returns the features a daemon at version v would drop. A
// too-old pin should be reported against these and not against every
// gated feature in the config: a 2.8.0 image honours
// attach.multi_session perfectly well, and listing it as a casualty
// teaches the reader to skim the list.
func (r Requirement) Unmet(v Version) []Reason {
	var out []Reason
	for _, reason := range r.Reasons {
		if v.Compare(reason.Min) < 0 {
			out = append(out, reason)
		}
	}
	return out
}

// Bullets renders reasons as an indented list for a failure message.
func Bullets(reasons []Reason) string {
	lines := make([]string, 0, len(reasons))
	for _, r := range reasons {
		lines = append(lines, r.String())
	}
	return "\n\t- " + strings.Join(lines, "\n\t- ")
}

// RequiredVersion returns the minimum daemon release cfg needs.
//
// "In use" is read off the config value by reflection rather than by a
// hand-written predicate per feature, so the table stays pure data and
// cannot disagree with the schema about what a field is called.
func RequiredVersion(cfg *config.Config) (Requirement, error) {
	var req Requirement
	for _, f := range GatedFeatures {
		min, err := ParseVersion(f.Min)
		if err != nil {
			return Requirement{}, fmt.Errorf("recipecheck: GatedFeatures[%q].Min: %w", f.Path, err)
		}
		used, err := pathInUse(cfg, f.Path)
		if err != nil {
			return Requirement{}, fmt.Errorf("recipecheck: GatedFeatures[%q].Path: %w", f.Path, err)
		}
		if !used {
			continue
		}
		req.Reasons = append(req.Reasons, Reason{Path: f.Path, Min: min, Why: f.Why})
		if req.Min.Compare(min) < 0 {
			req.Min = min
		}
	}
	return req, nil
}

// --- released versions ---

// changelogHeading matches a released section of CHANGELOG.md.
//
// The version shape is required, not merely started with a digit, and a
// heading that does not match is skipped rather than treated as an
// error. "## [Unreleased]" is the heading this has to skip today, but
// the general rule is the safe one: a changelog is prose, and a check
// that hard-fails the build on any heading it did not anticipate is a
// check that turns an editorial choice into a red build. Missing a
// version is caught by the "no headings at all" guard below and, for the
// versions that matter, by the fold trailer.
var changelogHeading = regexp.MustCompile(`^##\s+\[v?\d+\.\d+\.\d+(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?\]`)

// changelogHeadingVersion extracts the version out of such a heading.
var changelogHeadingVersion = regexp.MustCompile(`\[([^\]]+)\]`)

// FoldTrailerPrefix is the literal dev/release/cut-ga-tag.sh writes when
// it folds pre-release sections into a GA entry. See ReleasedVersions.
//
// TestFoldTrailerMatchesReleaseScript asserts this string still appears
// in cut-ga-tag.sh, so a reword there fails here instead of silently
// costing this check its memory of every dev tag.
const FoldTrailerPrefix = "_Pre-release history: cut incrementally as "

// foldTrailerTag matches one backticked tag inside that trailer:
// "`v2.8.0-dev.7` (2026-08-04), `v2.8.0-dev.6` (2026-08-04), ...".
var foldTrailerTag = regexp.MustCompile("`([^`]+)`")

// ReleasedVersions returns every version CHANGELOG.md records as
// released, newest first.
//
// # Why the changelog and not git tags
//
// The obvious oracle for "does this tag exist" is `git tag --list`, and
// this repo already has a presubmit that uses it. But that presubmit
// runs in a job whose checkout sets `fetch-depth: 0` specifically to
// make tags visible; the `test` job that runs `go test ./...` does not,
// and neither does a contributor's shallow clone, a `git archive`
// tarball, or a module extracted from the proxy. An oracle that answers
// "no versions exist" in those environments either red-builds the whole
// examples tree for an environmental reason or — far worse — gets a
// `len(tags) == 0` skip bolted onto it, at which point the check is a
// no-op exactly where nobody is watching. The changelog is a file in the
// tree; it reads the same everywhere.
//
// # What the changelog actually answers
//
// Strictly it answers "did someone write release notes", not "was an
// image published". Those coincide here by construction:
// dev/release/cut-dev-tag.sh and cut-ga-tag.sh promote [Unreleased] into
// a versioned section as part of the release commit, and
// release-images.yml publishes off the tag pushed with it. The gaps run
// in both directions and both are narrow:
//
//   - Permissive: the section lands in the release commit slightly
//     BEFORE the tag is pushed and the image is built, so for a few
//     minutes a version reads as released with no image behind it; an
//     abandoned cut would leave the same trace permanently.
//   - Restrictive: the fold, below.
//
// Neither is a reason to prefer no check. The failure this catches is a
// pin to "2.9.0" — a version that was never cut at all.
//
// # The fold
//
// cut-ga-tag.sh folds every pre-release section since the last GA into
// the new GA entry and DELETES those sections, which is correct for a
// human reader and would otherwise be a landmine here: the moment
// v2.9.0 GA is cut, "## [2.9.0-dev.1]" stops existing and every overlay
// pinned to it starts failing this check — on the release commit, in the
// required `test` job, on main. What saves it is that the fold writes
// the tags it removed into a machine-readable trailer, and this parses
// that trailer back. TestReleasedVersionsSurvivesTheGAFold is the
// regression.
//
// Sections written before that trailer convention existed (2.6.0 and
// earlier) have no such record, so their dev tags are not recoverable.
// That is acceptable: nothing in this repo can usefully pin a v2.6-era
// pre-release, and the failure is the loud direction.
func ReleasedVersions(changelogPath string) ([]Version, error) {
	body, err := os.ReadFile(changelogPath)
	if err != nil {
		return nil, fmt.Errorf("recipecheck: read changelog: %w", err)
	}
	var headings, folded []Version
	for _, line := range strings.Split(string(body), "\n") {
		switch {
		case changelogHeading.MatchString(line):
			m := changelogHeadingVersion.FindStringSubmatch(line)
			v, parseErr := ParseVersion(m[1])
			if parseErr != nil {
				// Unreachable: changelogHeading already asserted the shape.
				return nil, fmt.Errorf("recipecheck: %s: heading %q: %w", changelogPath, line, parseErr)
			}
			headings = append(headings, v)
		case strings.HasPrefix(strings.TrimSpace(line), FoldTrailerPrefix):
			vs, trailerErr := parseFoldTrailer(line)
			if trailerErr != nil {
				return nil, fmt.Errorf("recipecheck: %s: %w", changelogPath, trailerErr)
			}
			folded = append(folded, vs...)
		}
	}
	if len(headings) == 0 {
		return nil, fmt.Errorf("recipecheck: %s: no released version headings found; "+
			"the changelog format changed and this check has gone blind", changelogPath)
	}
	// Dedupe: a heading and a trailer should never name the same version
	// (the fold deletes the section it records), but "should never" is
	// not a reason to hand callers a list they have to defend against.
	seen := map[string]bool{}
	var out []Version
	for _, v := range append(headings, folded...) {
		if seen[v.canonical] {
			continue
		}
		seen[v.canonical] = true
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Compare(out[j]) > 0 })
	return out, nil
}

// parseFoldTrailer pulls the pre-release tags out of one fold trailer.
//
// Individually unparseable backticks are skipped — the trailer is a
// sentence and a future edit may well backtick something that is not a
// tag — but a trailer that yields no version at all is reported, because
// that means the format moved and this check just lost a release's worth
// of memory.
func parseFoldTrailer(line string) ([]Version, error) {
	var out []Version
	for _, m := range foldTrailerTag.FindAllStringSubmatch(line, -1) {
		if v, err := ParseVersion(m[1]); err == nil {
			out = append(out, v)
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("pre-release fold trailer names no parseable tag, so the "+
			"pre-releases it folded away are now invisible to this check: %q", line)
	}
	return out, nil
}

// isReleased reports whether v appears in the released set.
func isReleased(v Version, released []Version) bool {
	for _, r := range released {
		if r.Compare(v) == 0 {
			return true
		}
	}
	return false
}

// --- the config surface, by reflection ---

// ConfigSurface returns every JSON path in config.Config, sorted.
//
// Grammar: struct fields join with ".", a slice whose elements are
// structs contributes "[]", a map whose values are structs contributes
// "{}". A container of scalars is a leaf, since there is nothing
// underneath it to name. So the alert webhook env var is
// "alerts.targets[].url_env", a hook's command is "hooks{}[].command",
// and the disable list is just "tools.disable".
//
// This exists to make the GatedFeatures table's blind spot loud. A
// hand-maintained table's real failure mode is not a wrong row, it is a
// missing one — someone ships the next `alerts`-shaped block and never
// thinks about deployed daemons. Fingerprinting the surface turns that
// omission into a failing test on the PR that adds the field.
func ConfigSurface() []string {
	var out []string
	walkType(reflect.TypeOf(config.Config{}), "", map[reflect.Type]bool{}, func(p string) {
		out = append(out, p)
	})
	sort.Strings(out)
	return out
}

func walkType(t reflect.Type, path string, stack map[reflect.Type]bool, emit func(string)) {
	switch t.Kind() {
	case reflect.Pointer:
		walkType(t.Elem(), path, stack, emit)
	case reflect.Slice, reflect.Array:
		walkContainer(t.Elem(), path, "[]", stack, emit)
	case reflect.Map:
		walkContainer(t.Elem(), path, "{}", stack, emit)
	case reflect.Struct:
		walkStruct(t, path, stack, emit)
	default:
		emit(path)
	}
}

func walkContainer(elem reflect.Type, path, marker string, stack map[reflect.Type]bool, emit func(string)) {
	if !hasStructLeaf(elem) {
		emit(path)
		return
	}
	walkType(elem, path+marker, stack, emit)
}

func walkStruct(t reflect.Type, path string, stack map[reflect.Type]bool, emit func(string)) {
	if stack[t] {
		// A recursive config type would make the surface infinite. None
		// exists today; emitting a marker means one cannot arrive
		// silently.
		emit(path + ".<recursive " + t.String() + ">")
		return
	}
	stack[t] = true
	defer delete(stack, t)
	var named bool
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, ok := jsonName(f)
		if !ok {
			continue
		}
		named = true
		child := name
		if name == "" { // embedded struct: encoding/json inlines its fields
			child = path
		} else if path != "" {
			child = path + "." + name
		}
		walkType(f.Type, child, stack, emit)
	}
	if !named {
		// A struct with nothing addressable (or all fields skipped) is a
		// leaf as far as a config path is concerned.
		emit(path)
	}
}

// hasStructLeaf reports whether t bottoms out in a struct, so a
// container of scalars can be treated as a leaf.
func hasStructLeaf(t reflect.Type) bool {
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Map:
		return hasStructLeaf(t.Elem())
	case reflect.Struct:
		return true
	default:
		return false
	}
}

// jsonName returns the JSON name of a struct field, and false when the
// field is not serialized at all. An embedded struct with no json tag
// returns ("", true): encoding/json inlines its fields at the parent's
// level, and so does walkStruct.
func jsonName(f reflect.StructField) (string, bool) {
	if f.PkgPath != "" {
		return "", false // unexported
	}
	tag := f.Tag.Get("json")
	if tag == "-" {
		return "", false
	}
	name, _, _ := strings.Cut(tag, ",")
	if name != "" {
		return name, true
	}
	if f.Anonymous {
		return "", true
	}
	return f.Name, true
}

// --- "is this feature in use", by reflection ---

// pathInUse reports whether cfg's JSON asserted anything at the given
// config path. The rule is PRESENCE, read off the decoded value as
// precisely as each Go kind allows:
//
//   - A non-nil POINTER is present, whatever it points at. pkg/config
//     uses pointers exactly where absent and explicitly-false have to be
//     told apart (agent.auto_continue.enabled is the tristate #559
//     added), so `"auto_continue": {"enabled": false}` is an assertion —
//     an older daemon drops it, and dropping an opt-out is a behaviour
//     change even though every field in it is zero.
//   - A slice or map is present when non-empty. `"targets": []` asserts
//     nothing an older daemon could fail to honour: no target means no
//     `alert` tool on any release.
//   - A non-pointer struct is present when any field is.
//   - Anything else is present when non-zero.
//
// The one thing reflection cannot see is a non-pointer scalar written
// explicitly at its zero value — `"call_peer": {"enabled": false}`
// decodes identically to no call_peer block at all. That is a real limit
// and it is the harmless direction for every such field in the table
// today: an all-false block asks the daemon to do nothing, so an older
// daemon dropping it changes nothing. A field where the zero value IS an
// assertion must be a pointer in pkg/config, and if one ever is not, the
// fix belongs there rather than in a shadow schema here.
func pathInUse(cfg *config.Config, path string) (bool, error) {
	return valueInUse(reflect.ValueOf(*cfg), strings.Split(path, "."))
}

func valueInUse(v reflect.Value, steps []string) (bool, error) {
	if len(steps) == 0 {
		return present(v), nil
	}
	name, markers := splitMarkers(steps[0])
	v = deref(v)
	if !v.IsValid() {
		return false, nil
	}
	if v.Kind() != reflect.Struct {
		return false, fmt.Errorf("step %q: parent is a %s, not a struct", name, v.Kind())
	}
	f, ok := fieldByJSONName(v, name)
	if !ok {
		return false, fmt.Errorf("no JSON field %q on %s", name, v.Type())
	}
	return elementInUse(f, markers, steps[1:])
}

// elementInUse descends the "[]"/"{}" markers on one path step. A
// container is in use when ANY element is: one subagent carrying a root
// is enough to need the release that understands roots.
func elementInUse(v reflect.Value, markers []string, rest []string) (bool, error) {
	if len(markers) == 0 {
		return valueInUse(v, rest)
	}
	v = deref(v)
	if !v.IsValid() {
		return false, nil
	}
	switch markers[0] {
	case "[]":
		if k := v.Kind(); k != reflect.Slice && k != reflect.Array {
			return false, fmt.Errorf("marker \"[]\" on a %s", k)
		}
		for i := 0; i < v.Len(); i++ {
			ok, err := elementInUse(v.Index(i), markers[1:], rest)
			if ok || err != nil {
				return ok, err
			}
		}
	case "{}":
		if v.Kind() != reflect.Map {
			return false, fmt.Errorf("marker \"{}\" on a %s", v.Kind())
		}
		iter := v.MapRange()
		for iter.Next() {
			ok, err := elementInUse(iter.Value(), markers[1:], rest)
			if ok || err != nil {
				return ok, err
			}
		}
	default:
		return false, fmt.Errorf("unknown path marker %q", markers[0])
	}
	return false, nil
}

// splitMarkers peels the trailing "[]" / "{}" markers off a path step.
func splitMarkers(step string) (string, []string) {
	var markers []string
	for {
		switch {
		case strings.HasSuffix(step, "[]"):
			markers = append([]string{"[]"}, markers...)
			step = strings.TrimSuffix(step, "[]")
		case strings.HasSuffix(step, "{}"):
			markers = append([]string{"{}"}, markers...)
			step = strings.TrimSuffix(step, "{}")
		default:
			return step, markers
		}
	}
}

func fieldByJSONName(v reflect.Value, name string) (reflect.Value, bool) {
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		jn, ok := jsonName(f)
		if !ok {
			continue
		}
		if jn == name {
			return v.Field(i), true
		}
		if jn == "" { // embedded: its fields sit at this level
			if inner, found := fieldByJSONName(deref(v.Field(i)), name); found {
				return inner, true
			}
		}
	}
	return reflect.Value{}, false
}

// present implements the rule pathInUse documents.
func present(v reflect.Value) bool {
	switch v.Kind() {
	case reflect.Invalid:
		return false
	case reflect.Pointer, reflect.Interface:
		// Not `&& present(v.Elem())`: a pointer to a zero struct is the
		// tristate's "explicitly configured, all defaults", which is an
		// assertion the operator made and an older daemon would drop.
		return !v.IsNil()
	case reflect.Slice, reflect.Map:
		return v.Len() > 0
	case reflect.Struct:
		for i := 0; i < v.NumField(); i++ {
			if v.Type().Field(i).PkgPath == "" && present(v.Field(i)) {
				return true
			}
		}
		return false
	default:
		return !v.IsZero()
	}
}

func deref(v reflect.Value) reflect.Value {
	for v.Kind() == reflect.Pointer || v.Kind() == reflect.Interface {
		if v.IsNil() {
			return reflect.Value{}
		}
		v = v.Elem()
	}
	return v
}
