# Vendored A2A JSON Schema

`agentcard.schema.json` is a vendored copy of the Agent2Agent (A2A)
protocol JSON Schema. The card builder in `pkg/attach/agentcard.go`
emits documents that conform to the `#/definitions/AgentCard`
sub-schema; `agentcard_test.go::TestSchemaValidation` validates every
emitted fixture against it.

## Source

- Repo: https://github.com/a2aproject/A2A
- File: `specification/json/a2a.json`
- Tag:  `v0.3.0`
- URL:  https://raw.githubusercontent.com/a2aproject/A2A/v0.3.0/specification/json/a2a.json

The `main` branch no longer commits the JSON Schema — `a2a.proto` is
the single normative source and the JSON Schema is generated at build
time. We pin to the last committed tag (`v0.3.0`) so the fixture is
reproducible and reviewable.

## Refresh policy

Bump this file in a dedicated commit on each A2A minor-version bump:

1. Fetch the new schema:
   `curl -sSfL -o pkg/attach/testdata/agentcard.schema.json \
       https://raw.githubusercontent.com/a2aproject/A2A/<tag>/specification/json/a2a.json`
   (or regenerate from the proto if no JSON tag exists).
2. Update the **Tag** line above to the new version.
3. Bump `protocolVersion` in `pkg/attach/agentcard.go`'s emitted card
   to match.
4. Run `go test ./pkg/attach/...` — schema-validation tests surface
   any field in the card we emit that no longer matches.
5. Address fallout (rename fields, add/remove fields, update the
   wire-format pinning test).

The commit log of this directory doubles as our "what changed in A2A
and when" record.
