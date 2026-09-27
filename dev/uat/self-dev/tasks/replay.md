## This run is a replay

The task above has already been done upstream. This run grades the same
task from the commit it was cut at. Some instructions above assume
GitHub, and here they change:

- **The issue.** Don't use `gh issue view`. The issue, with every comment
  that existed when the task was cut, is reproduced below this section.
  `gh` has no credentials in this run.
- **Don't look up the upstream fix.** Don't browse, clone or fetch the
  repository from GitHub, and don't search for a later version of this
  code. The run is graded on your own fix. `origin` is a local mirror
  holding history up to the commit you cloned, and nothing later.
- **Push, but don't open a pull request.** Push your branch to `origin`
  as usual. Instead of `gh pr create`, write the pull request you would
  have opened to `.agents/logs/pr.md` in the clone: the title on the
  first line, then a blank line, then the body. Every rule above about
  the PR title and body applies to that file. It is gitignored, so leave
  it uncommitted. Your terminal state is the pushed branch plus that
  file.
- **The CHANGELOG** still cites the issue by its number.
