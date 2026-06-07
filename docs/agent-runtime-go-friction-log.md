# Friction log: deploying a Go agent to Google Cloud's Agent Engine / Agent Runtime

A first-hand log of what happens when you set out to deploy a
Go-based AI agent to Google Cloud's Agent Engine (renamed Agent
Runtime in 2026). Written June 2026 over a research session that
started with "how do we deploy core-agent to Agent Runtime?" and
ended with "Cloud Run is the only viable path for non-Python today."

**Audience:** anyone building agents in Go (or Rust, Node, etc.) who's
evaluating Agent Engine / Agent Runtime as a managed serving target.

**TL;DR.** As of June 2026, Agent Runtime is structurally Python-only.
The managed deployment path is cloudpickle-based at the wire level —
not just at the SDK level, but architecturally. The "BYOC"
(bring-your-own-container) escape hatch is Pre-GA, documented as
"Python only," and lacks any published HTTP wire contract. Even
Google's own Go ADK (`github.com/google/adk-go`) deploys to Cloud
Run, not Agent Runtime. Build for Cloud Run and use Agent Runtime's
data plane services (Sessions, Memory Bank) as client API calls if
you want them.

---

## Context

[core-agent](https://github.com/go-steer/core-agent) is a Go-based
agent runtime: a long-running daemon with HTTP attach, SQLite
session DB, A2A discovery card, OpenTelemetry export, ADC-based
auth to model providers. It already deploys cleanly to GKE
(`examples/gke-deploy/`). We had a concrete reason to evaluate
Agent Runtime: it bundles managed Sessions, Memory Bank, Code
Execution, Agent Identity, A2A fronting, and a governance layer
(Agent Gateway). Any of these would meaningfully simplify a
production agent. So: how do we deploy core-agent to Agent
Runtime?

The short version of what we found is in the TL;DR. The rest of
this doc is the receipts.

---

## Step 1: read the Agent Runtime overview

[Agent Runtime overview](https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/runtime)

Looked for: a section on language support.

Found: framework support tiers (ADK, LangChain, LangGraph,
LlamaIndex, AG2, CrewAI) — all Python. The page explicitly says
"install the latest version of the Agent Platform SDK for Python."
The "custom template" path is described as language-agnostic-ish:

> Agent2Agent (A2A) — Build interoperable agents that communicate
> and collaborate with other agents regardless of their framework.

A2A is the closest thing to a language-neutral integration path.
Container customization is mentioned: "Customize the agent's
container image with build-time installation scripts for system
dependencies." There's a BYOC option referenced under Sandbox.

**Friction:** Language support is implicit, not called out. You
have to read several pages and infer. The first signal that this
might be a problem is buried.

---

## Step 2: look for a Go SDK

[`cloud.google.com/go/agentplatform` on pkg.go.dev](https://pkg.go.dev/cloud.google.com/go/agentplatform)

Looked for: a Go SDK that could either build agents or wrap
container-side logic.

Found: the package exists, is real, and is purely **client-side**.

```go
client, err := agentplatform.NewGenAIClient(ctx, &genai.ClientConfig{...})
client.AgentEngines.Create(ctx, &types.CreateAgentEngineConfig{})
client.AgentEngines.Get(ctx, resourceName, nil)
client.AgentEngines.Delete(ctx, resourceName, nil)
```

Subclients: `Sessions`, `Sandboxes`, `Memories`, `MemoryRevisions`,
`SessionEvents`. Methods: `Create`, `Get`, `Delete`, `List`,
`Update`, `Query`, `Append`, `Generate`, `Retrieve`, `Purge`,
`Rollback`, `ExecuteCode`, and various `Get*Operation` for LROs.

What's not there: `Agent`, `AgentServer`, `Handler`, `Dispatcher`,
`Server`, `A2A`, `BYOC`, no `http.Handler`, no `grpc.Server`. No
`agentplatform/server` or `agentplatform/runtime` subpackage. The
package overview reads:

> New users are encouraged to use the Google GenAI Go SDK
> available at `google.golang.org/genai`.

Which is also client-only.

**Friction:** The Go SDK's client-only design reinforces that
Google's mental model is "Go talks TO Agent Runtime, doesn't RUN
ON it." This isn't a missing feature — it's the design.

---

## Step 3: read the BYOC docs

[BYOC setup](https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/runtime/setup#byoc)
and [Deploy an agent](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/deploy-an-agent)

Looked for: the container HTTP contract — port, endpoints,
request/response schema, auth flow.

Found: the BYOC setup page covers IAM bindings (two service
accounts need `roles/artifactregistry.reader`) and the
metadata-server tenant-project lookup. The deploy page lists
reserved environment variables the runtime injects: `PORT`,
`K_SERVICE`, `K_REVISION`, `K_CONFIGURATION`, `GOOGLE_APPLICATION_CREDENTIALS`,
`GOOGLE_CLOUD_PROJECT`, `GOOGLE_CLOUD_LOCATION`. Container
concurrency defaults to 9.

The `K_*` env vars are Cloud Run's container contract — strong
evidence that Agent Runtime BYOC runs on a Cloud Run-shaped
serverless layer underneath.

**What's missing:** Neither page documents:
- The exact port the container must listen on (just that `PORT`
  is "set by the runtime")
- The HTTP paths the container must serve
- The request/response JSON schema
- The auth flow Agent Runtime uses to call the container

The deploy page does, in passing, contain this one-liner:

> Agent Runtime deployment only supports Python.

Buried in a single sentence on page 4 of the docs.

**Friction:** BYOC is documented as a feature but not as a spec.
You cannot implement a BYOC container without the spec. The
"Python only" disclaimer is documented but easy to miss.

---

## Step 4: hunt for the wire contract in adjacent docs

Tried [identity](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/agent-identity),
[bidirectional streaming](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/bidirectional-streaming),
[A2A on Agent Runtime](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/use-an-a2a-agent),
[tracing](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/tracing),
[logging](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/logging).

The logging doc accidentally reveals the internal HTTP surface:

> You can also filter by runtime REST endpoints:
> - `POST /api/reasoning_engine` — sync/async methods
> - `POST /api/stream_reasoning_engine` — streaming/async streaming
> - `POST /api/bidi_reasoning_engine` — bidi streaming

The bidirectional streaming doc reveals the SDK-level dispatch
shape: methods register under three buckets — `""` (sync), `"stream"`,
`"bidi_stream"`. The A2A doc reveals that Agent Runtime fronts an
A2A agent at four public paths:

- `{a2a_url}/v1/card`
- `{a2a_url}/v1/message:send`
- `{a2a_url}/v1/tasks/{task_id}`
- `{a2a_url}/v1/tasks/{task_id}:cancel`

…and that "Agent Runtime does not serve the public agent card" —
so the standard A2A `/.well-known/agent-card.json` discovery path
is bypassed; clients use `handle_authenticated_agent_card()`
instead.

The identity doc reveals that agents get a SPIFFE-based identity
(`principal://TRUST_DOMAIN/NAMESPACE/AGENT_NAME`) with auto-provisioned
x509 certs and "certificate-bound tokens" — so inbound to the
container is likely mTLS, not just bearer JWT. End-user identity
propagation isn't documented.

**Friction:** The contract exists. Its endpoints leak through the
logging doc. Its dispatch buckets leak through the bidi doc. Its
auth model leaks through the identity doc. But none of these
pages document the actual request/response schema, the streaming
framing, or the mTLS handshake. The contract is referenced from
multiple places but never specified anywhere.

---

## Step 5: check if Go ADK exists

[`github.com/google/adk-go`](https://github.com/google/adk-go)

Looked for: a Google-blessed Go agent framework with a path to
Agent Runtime.

Found: an official Go ADK. Idiomatic Go, code-first, packages
include `agent/`, `artifact/`, `cmd/`, `examples/`, `memory/`,
`model/`, `plugin/`, `runner/`, `server/`, `session/`, `telemetry/`,
`tool/`.

The README's deployment section names exactly one target:

> Strong support for cloud-native environments like Google Cloud Run.

No mention of Agent Engine, Agent Runtime, or `reasoning_engines`.
The community tutorials follow suit — for example, [Building AI
Agents with the Go ADK](https://medium.com/google-cloud/building-ai-agents-with-the-go-agent-development-kit-adk-5a664fb39bf2)
walks through `make deploy` → Cloud Build → Cloud Run.

**Friction:** Google's own first-party Go ADK punts to Cloud Run
instead of Agent Runtime. That's the strongest possible signal
about where Go support stands.

---

## Step 6: reverse-engineer from the Python SDK

[`vertexai.agent_engines.AdkApp`](https://github.com/googleapis/python-aiplatform/blob/main/vertexai/agent_engines/_agent_engines.py),
[DeepWiki: Agent Engines and ADK](https://deepwiki.com/googleapis/python-aiplatform/9-agent-engines-and-adk),
[Deploy Your Agent Engine with Terraform](https://medium.com/google-cloud/deploy-your-agent-engine-with-terraform-the-enterprise-way-f918becff0c8).

If Google has a managed Python container that serves the
`/api/reasoning_engine` endpoints, it must have some abstraction
that hosts the user's Python code. Maybe we can extract the wire
contract from there.

Found: the managed deployment path is cloudpickle-based. When you
call `agent_engines.create(app, requirements=[...])`:

1. The SDK serializes the live Python `AdkApp` object with
   `cloudpickle.dump()`
2. Uploads three artifacts to a GCS staging bucket:
   - `agent_engine.pkl` (the cloudpickled object)
   - `requirements.txt` (pip deps for the container)
   - `dependencies.tar.gz` (any local Python `extra_packages`)
3. A Google-managed Python container pip-installs the requirements,
   then `cloudpickle.loads()` the bundle
4. The container introspects the loaded object via `OperationRegistrable`
   / `Queryable` / `StreamQueryable` / `BidiStreamQueryable` Python
   protocols
5. When an `/api/reasoning_engine` request arrives with
   `class_method=stream_query`, the container does
   `getattr(loaded_obj, 'stream_query')(**input_kwargs)`

This is Python all the way down. The container does pip + pickle
+ getattr — there is no plausible "port this to Go" because the
serialization format is Python's object graph.

**Friction:** The design itself rules out non-Python implementations.
This is not a documentation gap or a missing feature; it's an
architectural decision that excludes other languages by construction.

---

## Step 7: check the `[agent_engines]` pip extra for a server framework

[python-aiplatform `setup.py`](https://github.com/googleapis/python-aiplatform/blob/main/setup.py)

Last hope: maybe Google ships an "Agent Engine Functions Framework"
analogous to the Cloud Functions Framework — an open-source server
library that you base your container on. If so, we could port it
to Go.

Found: the `[agent_engines]` extra installs only client + telemetry
dependencies:

```
packaging >= 24.0
cloudpickle >= 3.0, < 4.0
google-cloud-trace < 2
google-cloud-logging < 4
opentelemetry-sdk < 2
opentelemetry-exporter-gcp-logging >= 1.11.0a0
opentelemetry-exporter-gcp-trace < 2
opentelemetry-exporter-otlp-proto-http < 2
pydantic >= 2.11.1, < 3
typing_extensions
google-cloud-iam
aiohttp
```

No `runtime`, `server`, `framework`, `byoc`, or `functions-framework`
package. The HTTP server lives inside Google's managed container
image, which isn't open-source. The package overview points users
who want a framework at `google.golang.org/genai` (which is also
client-only).

**Friction:** There is no Cloud-Functions-Framework-style library
you could port to Go (or any other language). The serving layer
is closed-source by design.

---

## Step 8: read the create-a-custom-agent docs

[Create a custom agent](https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/runtime/create-a-custom-agent)

Final check: the "custom agent" path is supposed to be the most
framework-neutral surface. Maybe BYOC works differently there.

Found: even the "custom" path is Python. The page is explicit:

> The constructor returns an object that must be 'pickle-able' for
> it to be deployable to Agent Runtime.

Methods are Python: `query(**kwargs)`, `stream_query(**kwargs) -> Iterable[dict]`,
plus async variants. Dispatch is by Python method name (defaults:
`query` and `stream_query`); override via:

```python
def register_operations(self):
    return {
        "": ["query", "get_state"],
        "stream": ["stream_query", "get_state_history"],
    }
```

The page does not mention BYOC, custom containers, or non-Python.

**Friction:** Even the documented framework-neutral escape hatch
is pickle-based Python. The escape hatch only escapes the
*framework*, not the *language*.

---

## What works (the actual answer)

**Cloud Run for compute. Agent Runtime's data plane services
(Sessions, Memory Bank, Code Execution) consumed as clients via
the Go SDK.**

This is the model Google's own Go ADK uses. It's GA, supported,
language-neutral, and unlocks most of what's interesting about
Agent Runtime without the language barrier.

Architecture:

| Capability | How a Go agent gets it |
|---|---|
| Compute / serving | **Cloud Run** (GA, Go-native, Google's own Go ADK does this) |
| Managed Sessions | Client call to `aiplatform.googleapis.com` via `cloud.google.com/go/agentplatform` |
| Memory Bank | Same — client API call from the Go SDK |
| Code Execution sandbox | Same |
| Agent Identity / governance | Agent Gateway in front of Cloud Run; Cloud Run supports workload identity for outbound auth |
| Observability | Cloud Trace + Cloud Logging via OpenTelemetry — works from Cloud Run unchanged |
| A2A interop | Implement A2A directly on Cloud Run's HTTP listener; standard A2A clients hit your Cloud Run URL |
| Discovery card | `/.well-known/agent-card.json` works on Cloud Run (unlike Agent Runtime, which bypasses the standard discovery path) |

See [`examples/cloud-run-deploy/`](../examples/cloud-run-deploy/)
for a working recipe.

---

## What would need to change at Google for non-Python languages to deploy ON Agent Runtime

1. **Publish the BYOC HTTP wire contract.** A spec — preferably
   OpenAPI / Protobuf — for `/api/reasoning_engine`,
   `/api/stream_reasoning_engine`, and `/api/bidi_reasoning_engine`.
   The JSON envelope for requests, the streaming framing (SSE?
   chunked transfer? newline-delimited JSON?), the error response
   shape, the auth flow (mTLS handshake details, end-user identity
   propagation, header names and claim names). Without a spec,
   language-neutral BYOC is fiction.

2. **Provide an open-source container framework.** Either
   open-source the Python container that today does
   pickle-load + introspect + dispatch, or ship a stand-alone
   "Agent Engine Functions Framework" library analogous to the
   [Cloud Functions Framework](https://cloud.google.com/functions/docs/functions-framework).
   Any language community could then port it. Even a Go reference
   implementation would unblock the ecosystem.

3. **Decouple managed Sessions / Memory Bank from the Python
   deployment model.** Currently you have to deploy a Python
   agent to Agent Runtime to "use" them — even though the SDKs
   (Python and Go) can call them directly via Vertex AI APIs.
   Document this as a supported pattern: "Sessions and Memory
   Bank are available to any Cloud Run / GKE / Cloud Functions
   workload via the Vertex AI SDK." That alone makes the managed
   data plane useful to the non-Python ecosystem.

4. **Update the documentation to be upfront about language
   support.** "Agent Runtime deployment only supports Python"
   currently appears in one paragraph of one page (the deploy-an-agent
   page). The overview pages talk about A2A as a "language-neutral
   path" without spelling out that A2A on Agent Runtime means
   Python-hosted A2A in Python containers. Put the constraint at
   the top of the overview page, with a clear pointer to Cloud
   Run for non-Python work.

5. **Move BYOC out of Pre-GA.** Pre-GA + officially-supported-Python-only
   + no published wire spec is three risk layers stacked. None
   alone is disqualifying; together they discourage anyone from
   investing in BYOC even for Python use cases, let alone
   non-Python ones.

6. **Add Go to the official ADK's deployment story.** The Go ADK
   already has `server/` and `runner/` packages. Wiring up Agent
   Runtime as a deployment target would close the loop. If that's
   not feasible because of (1) and (2), at minimum document the
   gap in the Go ADK README so users don't go looking.

---

## Postscript: why this matters

Most production agent code is Python today. That's fine.

But it's not the only language being used in agent work. Go has
core-agent, ADK-Go, ARK, and a handful of others; Rust has
Mistral.rs and the Ollama ecosystem; Node has LangChain.js and
Mastra; Java/Kotlin have several enterprise-shaped agent
frameworks. Making Agent Runtime de-facto Python-only excludes
those communities from the managed serving layer Google built.

The fix isn't to port Agent Runtime's Python runtime to other
languages — that's the wrong abstraction layer. The fix is to
make BYOC a real, specified, supported, GA product. Or, failing
that, to document the Cloud Run + managed-services-as-clients
pattern as a first-class deployment story instead of leaving
non-Python users to discover it themselves.

If you're reading this from Google's Agent Platform team: the
working pattern is in [`examples/cloud-run-deploy/`](../examples/cloud-run-deploy/).
It's not hostile to your platform — it consumes your data plane
services exactly as the Python ADK would. It just doesn't run on
your compute. That's an opportunity, not a problem.

---

## Sources

All claims in this log link to the source. Quick index of
everything cited:

**Google Cloud docs**

- [Agent Runtime overview](https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/runtime)
- [Agent Runtime setup (BYOC IAM)](https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/runtime/setup)
- [Deploy an agent](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/deploy-an-agent) — "Agent Runtime deployment only supports Python"; reserved env vars
- [Use an A2A agent](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/use-an-a2a-agent) — `/v1/card`, `/v1/message:send`, `/v1/tasks/{id}`, `/v1/tasks/{id}:cancel`
- [Create an A2A agent](https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/runtime/create-an-a2a-agent) — `on_message_send` / `on_get_task` / `cancel` / `handle_authenticated_agent_card`
- [Create a custom agent](https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/runtime/create-a-custom-agent) — pickle requirement, `register_operations`
- [Create an ADK agent](https://docs.cloud.google.com/gemini-enterprise-agent-platform/build/runtime/create-an-adk-agent) — `AdkApp` wrapper, session/memory builders
- [Agent identity](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/agent-identity) — SPIFFE, certificate-bound tokens
- [Bidirectional streaming](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/bidirectional-streaming) — `""` / `"stream"` / `"bidi_stream"` buckets
- [Tracing](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/tracing) — `GOOGLE_CLOUD_AGENT_ENGINE_ENABLE_TELEMETRY`, OTel semconv
- [Logging](https://docs.cloud.google.com/gemini-enterprise-agent-platform/scale/runtime/logging) — `/api/reasoning_engine`, `/api/stream_reasoning_engine`, `/api/bidi_reasoning_engine`

**SDKs and source**

- [`cloud.google.com/go/agentplatform`](https://pkg.go.dev/cloud.google.com/go/agentplatform) — Go SDK, client-only
- [`googleapis/python-aiplatform` `_agent_engines.py`](https://github.com/googleapis/python-aiplatform/blob/main/vertexai/agent_engines/_agent_engines.py) — `OperationRegistrable` / `Queryable` protocols
- [python-aiplatform `setup.py`](https://github.com/googleapis/python-aiplatform/blob/main/setup.py) — `[agent_engines]` and `[adk]` extras (no server framework)
- [DeepWiki: Agent Engines and ADK](https://deepwiki.com/googleapis/python-aiplatform/9-agent-engines-and-adk) — dispatch flow, file:line refs

**Go ADK**

- [`github.com/google/adk-go`](https://github.com/google/adk-go) — Cloud Run as the named deployment target
- [Building AI Agents with the Go ADK (Medium)](https://medium.com/google-cloud/building-ai-agents-with-the-go-agent-development-kit-adk-5a664fb39bf2) — `make deploy` → Cloud Build → Cloud Run

**Other**

- [Deploy Your Agent Engine with Terraform (Medium)](https://medium.com/google-cloud/deploy-your-agent-engine-with-terraform-the-enterprise-way-f918becff0c8) — three-file cloudpickle staging artifact
- [adk-python issue #1004](https://github.com/google/adk-python/issues/1004) — container-side `cloudpickle.loads()` traceback
- [Cloud Functions Framework](https://cloud.google.com/functions/docs/functions-framework) — example of the open-source serving framework that Agent Runtime needs
