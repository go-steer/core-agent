---
name: prefix-failure-verification
description: Prove a new regression test actually fails on the pre-fix code, without reverting files or checking out the parent commit. Use for any bug-fix PR in core-agent before opening it. Covers the patch-the-behaviour-back method, why the obvious methods do not work, predicting the failure list, and restoring cleanly.
---

# Proving the test fails before the fix

A test that passes on the buggy code is documentation, not a gate. This
exact failure has shipped in a downstream release, which is why every
bug-fix PR here records the pre-fix failure.

## Do not do it the obvious way

**Do not revert the production files. Do not check out the parent commit.**
Once the fix introduced a new const, type, field or signature, the test no
longer *compiles* against pre-fix sources — and a compile error is not
evidence about an assertion. You will have proved that the file changed,
which you already knew.

**Do not `git checkout -- <files>` to undo the patch.** If the fix is not
committed yet, `HEAD` is the pre-fix state and that command destroys the fix
itself. Restore from copies you made, or apply the inverse of the patch
script.

## The method that works

Used on #935, #974, #1036, #1137.

1. **Predict the failure list first**, in writing. Which tests, and roughly
   what each will say. A prediction you write afterwards is a description.
2. Copy each production file you are about to touch aside
   (`cp pkg/agent/foo.go /tmp/foo.bak`).
3. Patch the *behaviour* back **inside the new code**, leaving every new
   symbol in place so the package still builds. Silence "declared and not
   used" with `_ = theSymbol`. Mark every site:

   ```go
   // PREFIX BEHAVIOUR: #1137 — front-slot id removed, this is the #1136 splice.
   ```

   A script is better than hand edits here, because you need the inverse
   later and because you want the count to be exact.
4. Run the suite. **Record the exact failure lines verbatim** — they go in
   the PR body.
5. Restore from the copies.
6. Prove the restore: `grep -rn "PREFIX BEHAVIOUR" pkg/` must return nothing,
   and `git diff` against the staged tree must be empty. A test-cache hit on
   the re-run is good evidence the restore was byte-identical.

## Compare prediction to measurement

Say which tests failed and whether that matched. If a test you expected to
fail passed, that is the finding — it means the test does not exercise what
you thought, and it is worth more than the rest of the exercise.

A pure-function or boundary test that legitimately passes both ways is fine.
**Declare it in the PR** as "passes before and after, by design" rather than
quietly omitting it from the list.

## Mutation, for a guard rather than a bug fix

When the change adds a *check* rather than fixes a behaviour, the equivalent
step is mutation: break the tree on purpose in the way the check exists to
catch, confirm the check fires **on the right line with the right message**,
restore, verify the restore. Assert the message, not just the failure — a
check that fires for the wrong reason passes this exercise while catching
nothing.

Do both if the change does both.
