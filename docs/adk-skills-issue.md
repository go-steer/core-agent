<!--
Copyright 2026 Google LLC

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
-->

# Issue: Strict YAML Unmarshaling of `SKILL.md` Frontmatter Prevents Interoperability with Claude Skills 2.0 and Extended Metadata

### Summary
The Google ADK's `skill` package uses strict YAML unmarshaling when parsing `SKILL.md` files. Because of this, any unrecognized/custom frontmatter fields, or complex structures in fields like `compatibility` (e.g., maps of version constraints typical in **Claude Skills 2.0**), cause the entire parser to fail. Rather than ignoring unknown fields, this failure prevents the affected skill—and often the entire list of skills—from loading.

---

### Root Cause Analysis

In `google.golang.org/adk/tool/skilltoolset/skill`, the YAML metadata block is unmarshaled directly into a strict `Frontmatter` struct:

```go
type Frontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license,omitempty"`
	Compatibility string            `yaml:"compatibility,omitempty"`
	Metadata      map[string]string `yaml:"metadata,omitempty"`
	AllowedTools  []string          `yaml:"allowed-tools,omitempty"`
}
```

This strict unmarshaling triggers two distinct categories of failures when encountering modern community skill bundles:

#### 1. Failure on Unknown Fields (e.g., Claude Skills 2.0 Extensions)
Claude Skills 2.0 introduces custom frontmatter fields like:
* `user-invocable: true`
* `disable-model-invocation: false`
* `references: [ ... ]`

Because these fields are not defined in ADK’s `Frontmatter` struct, the parser fails with errors such as:
```text
yaml: unmarshal errors:
  line 3: field user-invocable not found in type skill.Frontmatter
  line 16: field references not found in type skill.Frontmatter
```

#### 2. Type Mismatch on `Compatibility`
In the ADK struct, `Compatibility` is strictly typed as a `string`. However, the open specification and practical `SKILL.md` files frequently structure compatibility as a nested YAML map of version requirements:
```yaml
compatibility:
  go: ">=1.20"
  charm.land/bubbletea: ">=v2.0"
```
This raises a type unmarshal error:
```text
cannot unmarshal !!map into string
```

---

### Proposed Fixes in the ADK

To ensure maximum interoperability and make the ADK resilient to future spec changes, the following modifications should be made to the ADK `skill` parser:

#### 1. Relax YAML Unmarshaling Constraints
Ensure the YAML unmarshaler does not throw errors on unrecognized properties. 
* If utilizing `gopkg.in/yaml.v3`, ensure strict unmarshaling (like `UnmarshalStrict`) is not enabled on the stream, allowing unknown keys to be ignored gracefully.

#### 2. Support Complex Types for `Compatibility`
Implement a custom unmarshaler for `Compatibility` (or represent it internally as a generic interface/value before processing) so it can accept both basic string values and nested map structures.

**Example custom unmarshal pattern:**
```go
type Frontmatter struct {
	Name          string            `yaml:"name"`
	Description   string            `yaml:"description"`
	License       string            `yaml:"license,omitempty"`
	Compatibility Compatibility     `yaml:"compatibility,omitempty"` // Custom type
	Metadata      map[string]string `yaml:"metadata,omitempty"`
	AllowedTools  []string          `yaml:"allowed-tools,omitempty"`
}

type Compatibility string

func (c *Compatibility) UnmarshalYAML(value *yaml.Node) error {
	// If it is a scalar string, unmarshal it directly
	if value.Kind == yaml.ScalarNode {
		var s string
		if err := value.Decode(&s); err != nil {
			return err
		}
		*c = Compatibility(s)
		return nil
	}
	
	// If it is a map or sequence, marshal it back to a string representation 
	// or extract the raw string block to prevent parsing failures.
	var raw any
	if err := value.Decode(&raw); err != nil {
		return err
	}
	bytes, err := yaml.Marshal(raw)
	if err != nil {
		return err
	}
	*c = Compatibility(bytes)
	return nil
}
```
