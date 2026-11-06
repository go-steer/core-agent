@include builtin:core

## Proposing a GitOps change (`propose-change`)

When `vcs_*` tools are registered in your tool list, propose declarative
configuration or manifest changes through the configured GitOps repository
using this workflow:

1. **Inspect capabilities.** Call `vcs_capabilities` on the target repository.
   If `proposal` is reported as `"missing"` and your task requires opening a
   proposal, stop and report that the configured forge does not support
   proposals (or report the published branch if a branch deliverable satisfies
   the task).
2. **Clone at the base branch.** Call `vcs_clone` for the repository (leaving
   `branch` empty or passing `base_branch` from `vcs_capabilities`).
3. **Edit and verify the diff.** Modify only the targeted files inside the
   cloned `path` using `read_file`, `edit_file`, or `write_file`. Run
   `vcs_diff` and `vcs_status` to confirm the working tree contains the
   diagnosed change and nothing else.
4. **Commit the change.** Call `vcs_commit` with a commit `message` (at least
   10 characters) that names the finding and the affected resource (and an
   optional topic `branch` starting with `branch_prefix`).
5. **Publish to a new agent branch.** Call `vcs_publish` with a fresh `branch`
   name starting with the configured `branch_prefix` (for example,
   `agent/<slug>-<shortid>`). Never pass the repository's base branch.
6. **Open the proposal.** Call `vcs_propose` with that `source_branch`, a
   concise `title`, and a `body` containing the root-cause diagnosis, the
   evidence collected in this session, and the rollback step.
7. **Report the proposal.** Return the proposal number and URL in your final
   response (or via `alert` / `return_result` when registered).
