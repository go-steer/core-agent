# Context Window Compaction & Management: Crush & Antigravity

This document outlines the architectural patterns and implementations used to manage large contexts, prevent token overflow, and sustain deep continuity during complex agentic workflows. It covers both **Crush** (the interactive TUI assistant) and **Antigravity** (your pair-programming AI agent).

---

## 1. Crush: Session Compaction & Resume Flow

Crush implements a highly robust, database-backed, auto-summarizing compaction system to manage the context window during active terminal sessions. This architecture is defined in [agent.go](file:///home/user/projects/tui-skills/crush-src/internal/agent/agent.go).

### A. Threshold Monitoring & Triggering
During a session turn, Crush checks the remaining context budget after every step within its execution loop:
* **Threshold Buffer**:
  * **Large Context Models** (context window > 200,000 tokens): Uses a fixed safety buffer of **20,000** tokens.
  * **Standard Context Models** (context window $\le$ 200,000 tokens): Uses a buffer equal to **20%** of the model's total context window.
* **Skipping**: If the context window limit is reported as `0` (such as with local/custom models), auto-summarization is skipped entirely to prevent premature trimming.
* **Stop Condition**: When the remaining tokens drop below the threshold, the active execution loop is halted, and `shouldSummarize` is set to `true`.

### B. Interruption & Prompt Queueing
When a session is forced to stop due to context exhaustion while the agent has outstanding tool calls, Crush ensures continuity:
* It queues the remaining work back into the session's prompt queue (`a.messageQueue`).
* It modifies the prompt with context-restoring metadata:
  ```
  The previous session was interrupted because it got too long, the initial user request was: '<original_prompt>'
  ```

### C. The Summarization Turn (`Summarize`)
Once halted, the session agent is temporarily locked, and Crush initiates the summarization process:
* **The Summarizer Prompt**: It runs a dedicated, detailed call using the large LLM with instructions from [summary.md](file:///home/user/projects/tui-skills/crush-src/internal/agent/templates/summary.md).
* **Mandatory Sections**: The model is instructed to build a teammate-style debrief with the following sections:
  1. **Current State**: Exact user request, progress completed, active work, and specific remaining tasks.
  2. **Files & Changes**: Modified files (with descriptions), read/analyzed files, untouched files needing changes, and important code locations with line numbers.
  3. **Technical Context**: Architectural decisions, patterns, successful/failed commands, and environment details.
  4. **Strategy & Approach**: Chosen strategy, alternatives, gotchas, assumptions, and blockers.
  5. **Exact Next Steps**: A concrete numbered list of specific developer actions.
* **Storage**: The generated summary is persisted in the SQLite database with `IsSummaryMessage: true`, and the session’s `SummaryMessageID` is set to this message's ID.

### D. Trimming Active Context (`getSessionMessages`)
On all subsequent turns, when retrieving history for the LLM input, Crush truncates older messages by leveraging the saved summary message:
```go
if session.SummaryMessageID != "" {
    summaryMsgIndex := -1
    for i, msg := range msgs {
        if msg.ID == session.SummaryMessageID {
            summaryMsgIndex = i
            break
        }
    }
    if summaryMsgIndex != -1 {
        msgs = msgs[summaryMsgIndex:]
        msgs[0].Role = message.User
    }
}
```
* **History Slicing**: All messages prior to the summary are discarded.
* **Role Transition**: The summary message itself has its role mutated to `message.User`. This ensures that the resuming model receives the comprehensive summary as its initial starting prompt, preserving full project continuity.
* **Token Reset**: `PromptTokens` is reset to `0`, and `CompletionTokens` is set to the token size of the summary itself, lowering resource usage.

### E. Manual Compaction
Users can manually trigger this compaction process on demand directly from the Crush TUI command menu. Selecting the `summarize` option triggers `AgentSummarizeSession` via the workspace client, which executes the exact same flow described above.

---

## 2. Antigravity: Context & State Management

Antigravity operates within the Advanced Agentic Coding platform. It uses both platform features and structural workspace habits to maintain a lean, high-fidelity context window across long, multi-turn pair programming sessions.

### A. Platform-Level Context Management
* **Automatic Truncation**: As the conversation grows, the platform manages context length limits by automatically truncating older turns of our chat history. This keeps the input window within safe token bounds and ensures fast response times.
* **Transcript Logs**: For deep-history recall, Antigravity has read access to the locally written conversation transcripts:
  * Compact transcript: `<appDataDir>/brain/<conversation-id>/.system_generated/logs/transcript.jsonl`
  * Full transcript: `transcript_full.jsonl`
  These files are in JSON Lines (JSONL) format, allowing Antigravity to use ripgrep/file-viewing tools to search for and restore precise context from earlier in the session.

### B. Isolated Subagent Execution (`invoke_subagent`)
To avoid polluting its primary conversation history with massive logs, search outputs, or multi-step directory analyses, Antigravity utilizes specialized subagents:
* **`research`**: A read-only subagent that runs complex code searches and reads large documentation files in a separate context.
* **`self`**: A cloned context to verify or implement a parallel thread of work.
These subagents execute their tasks concurrently in independent, sandboxed context windows and report back only the polished results, keeping the master context clean.

### C. State Externalization (Workspace Artifacts)
To prevent the main conversation context from being overwhelmed with tracking code changes, checklists, and plan revisions, Antigravity uses markdown **Artifacts** to keep state externalized:
* **`implementation_plan.md`**: Outlines goals, architecture, proposed changes, and verification plans to obtain user approval before any code changes are made.
* **`task.md`**: Maintains an active checklist (`[ ]`, `[/]`, `[x]`) updated continuously during execution.
* **`walkthrough.md`**: Captures final code diffs, screenshot embeddings, and validation results.

By storing task states in persistent workspace documents rather than the active chat history, Antigravity can work across dozens of turns without experiencing "attention decay" or token limits.

---

## 3. Antigravity: Subagent Delegation Directives

To protect the main conversation thread from context bloating, the Antigravity system prompt contains specific directives detailing exactly how and when to spawn, communicate with, and tear down subagents.

### A. How Subagents are Managed
* **Spawning**: Antigravity invokes background agents via the `invoke_subagent` tool. New custom subagents can also be defined dynamically mid-conversation with the `define_subagent` tool.
* **The `send_message` Communication Tool**:
  * **Tool Signature**:
    * `Recipient` (string): The unique conversation ID representing the target subagent (returned upon subagent invocation).
    * `Message` (string): The raw text or structured string payload (instructions, queries, feedback, or data boundaries) being dispatched.
  * **Agent-to-Agent Rule**: The `send_message` tool is strictly reserved for communicating with other active subagents (e.g., `research`, `self`). It is **never** used to communicate with the human operator. Conversing with the user is done exclusively via direct markdown text responses in the active thread.
* **Event-Driven Reactive Wakeup**:
  * **State & Flow**: Spawning subagents and sending messages are non-blocking processes. Instead of running expensive busy-polling loops (such as executing shell `sleep` commands or polling in a loop) to wait for a subagent to reply, the parent agent can continue other local tasks (editing files, running tests) in the same turn, or choose to stop calling tools to suspend execution.
  * **Automatic Resumption**: When a recipient subagent responds or a background task completes, the platform triggers a high-priority wakeup notification. The parent agent's execution loop is automatically resumed, and the reply is injected directly into its context. This optimizes resource usage and prevents KV-cache invalidation from idle "waiting" turns.
* **Architectural Similarities to Crush**:
  * This protocol mirrors the subagent coordinator in Crush (`coordinator.go`), where tasks are run non-interactively via `runSubAgent(...)`.
  * Crush manages subagent communication programmatically via database-backed message queues (`a.messageQueue`) in SQLite, tracking and propagating token/financial costs back to the parent session upon sub-session completion.


### B. Decision Guidelines for Spawning Subagents
Antigravity selects subagents strategically based on task complexity and resource isolation goals:

1. **The `research` Subagent** (Read-Only)
   * **Purpose**: Exploring codebases, reading files, and performing web searches.
   * **When to Spawn**:
     * When a research task is complex and requires dozens of search or file-viewing operations that would otherwise **clutter the primary context window** with raw text.
     * When performing a **broad architectural survey** or examining massive project documentation.
     * When wanting to offload discovery tasks to the background while Antigravity simultaneously codes, builds, or tests in the main context.
   * **When to Avoid**: Quick, targeted lookups must be executed by the main agent directly to avoid unnecessary resource usage and extra latency.

2. **The `self` Subagent** (Full Capabilities Clone)
   * **Purpose**: Parallel execution or isolated code writing.
   * **When to Spawn**: When a task requires writing code or running commands in a separate, completely **isolated workspace or context window**, while fully retaining the parent agent's system instructions, memories, and toolset.

---

## 4. Crush: Programmatic Subagent Delegation

Crush implements an exceptionally similar programmatic subagent delegation pattern to coordinate distinct tasks, optimize model costs, and prevent terminal user prompt clutter.

### A. Core Subagent Support (`runSubAgent`)
The Crush coordinator ([coordinator.go](file:///home/user/projects/tui-skills/crush-src/internal/agent/coordinator.go)) implements `runSubAgent(...)` to manage child tasks:
* **Session Forking**: It creates a dedicated task-level sub-session in SQLite linked to the parent session.
* **Non-Interactive Execution**: Subagents run with `NonInteractive: true` to avoid interrupting the terminal user.
* **Cost Propagation**: On completion of the sub-task, the cumulative financial and token costs of the child session are propagated and saved back to the parent session for exact usage tracking.

### B. The `agent` Tool (Task Delegation)
Defined in [agent_tool.go](file:///home/user/projects/tui-skills/crush-src/internal/agent/agent_tool.go), this tool allows the main LLM to spawn a generic background agent (`config.AgentTask`) to work through a separate, isolated task and return a refined, digested text response.

### C. The `agentic_fetch` Tool (Read-Only Information Digestion)
To browse the web, crawl files, or run grep queries without bloating the primary coder's context window, Crush uses a specialized read-only subagent inside [agentic_fetch_tool.go](file:///home/user/projects/tui-skills/crush-src/internal/agent/agentic_fetch_tool.go):
* **Model Downscaling**: It configures the subagent to run on a cheaper, faster `smallModel` (as high-level reasoning is not required for fetching).
* **Targeted Tooling**: It limits the subagent's tools to a small, read-only set: `web`, `glob`, `grep`, `sourcegraph`, and `view`.
* **Hook Bypassing**: To avoid firing user authorization prompts repeatedly for every internal search/view query, subagent tools skip standard terminal hooks (`wrapToolsWithHooks` is bypassed if `isSubAgent` is true).
* **Auto-Approval**: The coordinator configures the sub-session to auto-approve read operations (`AutoApproveSession(sessionID)`), allowing the subagent to run silently and quickly before returning a clean summary of the fetched content.

---

## 5. Comparative Case Study: Anthropic's Claude Code Compaction

Anthropic's CLI agent, **Claude Code**, implements a robust compaction architecture to manage its context window, offering an insightful industry comparison to Crush and Antigravity.

### A. Automatic & Manual Compaction
* **Auto-Compaction**: Triggered automatically when context window usage reaches a specific threshold (typically around **85%** of capacity). It summarizes older blocks of the conversation while keeping the most recent messages verbatim to maintain immediate momentum.
* **Manual Compaction (`/compact`)**: Users can proactively initiate compaction at natural task boundaries (e.g., after completing a feature).
* **Targeted Summarization**: Unlike standard summaries, the `/compact` command accepts prompt arguments (e.g., `/compact focus on latest API changes`) to explicitly instruct the summarizer on which details to prioritize and retain.

### B. Persistent Memory Reinjection (`CLAUDE.md`)
Because summarizing history inevitably degrades technical nuances, Claude Code leverages a designated **`CLAUDE.md`** file in the repository root:
* **Always-On Context**: The agent automatically reads `CLAUDE.md` at startup and injects its contents into the prompt on *every single turn*. Since this file survives compaction, it serves as the ultimate source of persistent rules and architecture constraints.
* **`Compact Instructions` Section**: Users can include a specific `Compact Instructions` header in their `CLAUDE.md` to feed explicit guidelines directly to the summarizer model whenever a compaction turn occurs.

### C. Comparison with Crush & Antigravity
* **Manual Commands**: Both Crush (`summarize` in TUI) and Claude Code (`/compact` CLI command) allow user-initiated compaction.
* **Auto-Triggering**: Crush triggers auto-compaction based on remaining buffer size (e.g., 20k tokens or 20% limit), whereas Claude Code triggers based on percentage-filled thresholds (~85%).
* **Context Truncation**: Crush and Claude Code both discard raw messages prior to the summary, placing the generated summary at the beginning of the context (mutating its role to act as the primary starting point).
* **State Preservation**: Claude Code relies heavily on `CLAUDE.md`, while Crush uses `AGENTS.md` (via `.cursorrules`/`claude.md` template lookups in `initialize.md.tpl`) and persistent `Todos` tool states to guarantee core technical guidelines survive the summary turn.

---

## 6. Industry Best Practices for Context Optimization

Beyond the specific implementations seen in Crush and Claude Code, several advanced, industry-standard patterns have emerged for context window optimization and compaction in production agentic systems.

### A. Segmented Memory Architecture
To avoid the cognitive decay that occurs when models are fed flat, undifferentiated chat histories, high-performance agents divide their context into three distinct memory tiers:
1. **Working Memory (Verbatim Core)**: Retains the immediate last 3 to 5 turns of conversation and tool responses exactly as they occurred. This preserves immediate context, conversation style, and formatting cues.
2. **Semantic Memory (Structured State)**: Consolidates high-level project information, structural assumptions, and rule lists into a centralized, persistent file or database (e.g., a dynamic state artifact or a `memory.json`).
3. **Episodic Memory (Vector-Indexed Retrieval)**: Summarizes older turns and logs, indexing them into a local vector database or disk archive. Rather than keeping these in the active window, they are queried via RAG and reinjected *only* when the agent or user searches for historic decisions.

### B. KV-Cache & Prompt Caching Optimization
Modern large language models charge significantly less for cached inputs (e.g., Claude's Prompt Caching, Gemini's Context Caching). To maximize cash savings and reduce latency, context layouts are structured around physical caching boundaries:
* **The Static Header**: Global instructions, system prompts, workspace rules (`AGENTS.md`, `CLAUDE.md`), and directory listings are grouped at the very beginning of the prompt. Because these remain unchanged across turns, they are cached indefinitely.
* **The Dynamic Tail**: High-frequency changes—such as the sliding conversation history, latest tool outputs, and the active user prompt—are pushed to the very end of the input stream. This layout prevents dynamic changes from invalidating the cache of the static header, resulting in up to 90% faster times-to-first-token.

### C. Tool-Output Encapsulation & Truncation
Raw tool outputs (such as massive `grep` results, bundler logs, or unit test traces) are the single largest source of context bloat. Industry leaders mitigate this through selective filtering:
* **Head/Tail Truncation**: When tool outputs exceed a reasonable line limit (e.g., 100 lines), wrappers automatically truncate the middle portion, returning only the first 30 and last 50 lines alongside an explicit note (e.g., `[... 820 lines of build output omitted ...]`).
* **Semantic Error Extraction**: Prior to appending command logs to the history, output filters process the raw logs with regular expressions or lightweight parsing utilities to extract *only* traceback details, compiler errors, and warning lines, discarding thousands of lines of successful compilation logs.

### D. Token-Level Selective Pruning (Weeding)
Rather than executing a hard truncation of older turns, selective message pruning programmatically removes verbose, intermediate tool payloads (such as raw file read contents or search indices) from older history items while preserving the user's instructions and the agent's concluding thoughts. This maintains a clean, coherent, and unbroken conversational thread while reclaiming massive amounts of context space.

### E. Task-Boundary State Resetting
When an agent completes a major task (e.g., a checkbox item in `task.md` or a feature branch merge), the agentic platform can trigger a session reset. The current working directory's modified state is committed or cached, the active conversation history is completely cleared, and a new session starts with a fresh context window. A concise bootstrap prompt is injected to introduce the next objective, allowing the model to work with 100% attention capacity.

---

## 7. Future Horizons: Advanced State & Coordination Frontiers

As terminal user interfaces and developer assistants evolve, several next-generation patterns represent the cutting edge of state, context, and multi-agent coordination research.

### A. Tool Idempotency & Interruptibility
* **Concept**: A multi-turn refactoring sequence may be abruptly halted due to token exhaustion, transient API timeouts, or human cancellation. If the agent resumes, re-running previous tool calls blindly can cause syntax errors (such as duplicate function appends or broken imports).
* **Implementation Standard**:
  * Build **idempotent modification tools** (such as regex-based search-and-replace rather than offset-based insertion) so that executing the same tool call twice results in the identical file state without side effects.
  * Maintain a local **transaction-style checkpoint database** (such as SQLite). Before each file write or shell command is executed, the agent logs the action status. Upon session resumption, the agent scans the log to skip already-executed operations, preserving continuity seamlessly.

### B. Runaway Execution & Token Budget Gating
* **Concept**: To prevent recursive research subagents, infinite compile-and-fix loops, or broad web crawls from incurring immense financial and latency bills, parent agents must govern subagent actions.
* **Implementation Standard**:
  * **Dynamic Token Quotas**: Pass custom budget structures during subagent invocation (e.g., `{"max_tokens": 150000, "max_cost_usd": 0.50}`). If the sub-session exceeds these boundaries, the coordinator automatically terminates the run.
  * **Turn-Count Enforcers**: Impose a hard ceiling on tool execution loops (typically 5 to 7 turns) for background subagents. Upon hitting the ceiling, the subagent is forced to halt and return its best-effort progress report rather than crashing or running endlessly.

### C. Model-Agnostic Tool Translation (Adapter Layer)
* **Concept**: Advanced cloud models leverage structured JSON schema definitions for native tool-calling, while smaller or local models (e.g., Llama-3 8B, Mistral 7B) running locally inside a developer's TUI might require custom XML delimiters or markdown blocks.
* **Implementation Standard**:
  * Build a robust **tool-adapter translation layer** inside the client coordinator. This abstraction layer maps standardized internal JSON tool event structures into the specific syntax required by the active model (converting to XML tags or text schemas where necessary) and normalizes the parsed responses back to a single universal format.

### D. Human-in-the-Loop (HITL) Permission Escalation
* **Concept**: Subagents execute in background, non-interactive environments to avoid disrupting the developer. However, they may encounter actions that demand security/permission gates (e.g., running untrusted shell binaries, making HTTP requests, or deleting directories).
* **Implementation Standard**:
  * **Deferred Resolution**: When a subagent encounters a gated tool call, it registers a "pending authorization event" with the parent coordinator and yields execution.
  * **Interactive Escalation**: The coordinator intercepts the yield and displays an interactive approval card directly in the developer's main TUI viewport. The developer can review the exact command, approve or reject the request, and the coordinator transparently resumes the subagent with the authorization token.

### E. Viewport-Aware Prompt Injection
* **Concept**: While system prompts are static, the developer's current visual focus (what line range or file they are actively editing/viewing on their terminal screen) is a powerful, implicit signal of task relevance.
* **Implementation Standard**:
  * **Active Focus Syncing**: The terminal UI dynamically tracks the terminal viewport's state and automatically injects active view context as temporary, high-priority XML tags into the prompt header of the current turn:
    ```xml
    <viewport_focus file="./internal/ui/viewport.go" start_line="45" end_line="85" is_focused="true" />
    ```
  * This allows the model to align its attention with the developer's visual field, eliminating the need for the developer to manually type out which file sections they are currently referencing.





