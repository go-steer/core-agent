# UAT: Agent Engine Sessions transcript

Manual, against a real Vertex AI Agent Engine instance. Proves the one
thing `state.transcript.backend: "agentengine"` (#1192 phase 3) is for:
**the session transcript outlives the local SQLite file.**

## Prerequisites

- A reasoning engine, created without deploying code:
  `gcloud ai reasoning-engines create --location=L --display-name=core-agent-sessions`
  (or the console). Its full resource name goes in
  `CORE_AGENT_TEST_REASONING_ENGINE`.
- ADC for a principal with `roles/aiplatform.user` on the project:
  `gcloud auth application-default login`.
- A model the daemon can reach: `GEMINI_API_KEY`, or set `UAT_MODEL_JSON`
  to a Vertex model block.
- `sqlite3` on the PATH (used only to read the session ID back).

## Run

```bash
CORE_AGENT_TEST_REASONING_ENGINE=projects/P/locations/us-central1/reasoningEngines/N \
  dev/uat/agentengine-transcript/run.sh
```

Steps, each fatal on failure:

0. The live conformance suite (`TestAgentEngine_Conformance_Live`) —
   ordering, filters, handover, field fidelity, the pending-operation
   create path and the `userId=` filter spelling, against the real
   service.
1. Build `core-agent` into `/tmp/core-agent-uat/agentengine/bin`.
2. Write a config pinned with `-c`: `agentengine` transcript, local db
   under `/tmp`.
3. One headless turn with a tool call. The startup log must say
   `session store: agentengine …` and
   `session db: … [implied by state.transcript.backend=agentengine …]`.
4. **Delete the SQLite file.**
5. `cmd/restore` opens the backend with no local state and prints the
   tree: every event with its seq, author, text and token counts. The
   model's reply and `UsageMetadata` must be there.

## What it does not prove (yet)

Attach-level resume on a fresh machine: the attach server's lazy resume
reads the session ACL row from the local SQLite (`compose.
BuildSessionResumer`), so `GET /sessions/{app}/{sid}` on a new sandbox
answers 404 until the index moves off SQLite (#1192 phase 4). The
transcript is there; the door is not. That is why step 5 restores
through the library.

## Record the run here

| Date | Engine location | Events/turn | Largest event accepted | Quota or size rejections | Notes |
|---|---|---|---|---|---|
| | | | | | |
