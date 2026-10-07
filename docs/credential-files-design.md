# Credential files and a root daemon (#1201 items 4 and 5)

#1157 kept the daemon's env-var credentials out of every process it
starts and made the daemon non-dumpable while it holds one. Its
adversarial review found routes outside the daemon's process; #1201
tracks them. This doc settles two:

- **Item 4.** In multi-session mode the attach credential is the bearer
  table, `attach.multi_session.auth.table_file`. It is a plaintext file
  the daemon reads, so the daemon's user can read it, and the agent runs
  as that user. File modes separate users, not a daemon from its own
  tools. A model that reads the table can answer its own permission
  prompts. A model that can write it can add its own token for the next
  boot.
- **Item 5.** `PR_SET_DUMPABLE` stops only processes without
  `CAP_SYS_PTRACE`. A daemon running as root passes its bounding set to
  every child as that child's permitted set. A child holding
  `CAP_SYS_PTRACE` can read the daemon's `/proc/<pid>/environ` and
  memory regardless of the flag.

Items 2 and 3 (token-less listeners, `--token-file`) are separate work.

## What is enforced and what is only warned

| Route | Status | Where |
|---|---|---|
| Agent file tools (`read_file`, `write_file`, `edit_file`, `delete_file`, `grep`, `glob`, `read_many_files`, `json_query`, `view_file_outline`, `list_dir`, `stat`) on a credential file, by any path: lexical, or anything that is the same file *now* — a symlink, a hard link, or the new generation after a Kubernetes Secret update | **Enforced.** Refused in every mode, yolo included, before scope, grants and the prompter. `grep`, `glob` and `read_many_files` walks skip the file; `list_dir` of its directory still shows its name. | `permissions.Gate.credentialFileDenial`, from `CheckFileRead` / `CheckFileWrite`; matching in `childenv.FileSet` |
| An `@include` of the file, or an `AGENTS.d/` entry that is a link to it, in any instruction file — the route an agent holding only `write_file` has, by editing `AGENTS.md` | **Enforced.** The load fails, naming the file, so neither the boot nor a `POST /sessions` gets a prompt with the table in it. | `instruction.loadFile`, consulting `childenv.WithheldFile` |
| Agent `bash` command that names the file directly: absolute path, resolved path, or path relative to the bash tool's working directory, as a whole shell word | **Enforced, as a seatbelt.** Same refusal. Hook commands go through the same check. | `permissions.Gate.credentialBashDenial`, from `checkBash` |
| Agent `bash` reaching the file any other way (`cd dir && cat users.json`, a glob, a variable, a script it wrote) | **Warned.** No check of the command text can close this. | `credentialFileExposure`, at startup |
| The table's contents in the daemon's memory, read through `/proc/<pid>/mem` by a same-user child | **Enforced.** The daemon is made non-dumpable whenever a credential file is configured, as #1157 does for an env credential. | `guardCredentialFiles` |
| A daemon with real or effective uid 0 whose bounding or inheritable set holds `CAP_SYS_PTRACE`, or any daemon whose ambient set does, while it holds a credential | **Warned**, loudly. Startup is not refused. | `childenv.PtraceWarning`, from `guardCredentialFiles` |

The warning for a file fires when the daemon's user can read the file
and either `bash` is registered or the file sits inside the agent's
path scope. `bash` is not path-scoped. The gate sees a command string,
not the files the shell opens. So for `bash`, "inside the agent's
reach" means "readable by this user", wherever the file is mounted. An
in-scope placement adds one risk of its own: directory listings show
the file to the model, and anything that reads files without the gate's
path check finds it in the working tree. That includes an MCP
filesystem server and a skill script run through `bash`.

## Settled decisions (do not relitigate)

1. **The credential-file set comes from config, inside
   `permissions.FromConfig`.** `config.CredentialFiles()` lists it, and
   every gate built from that config protects those files. No host
   wiring site can forget the call, and AGENTS.md's getter pitfall
   cannot arise, because the set exists before any gate is derived. It
   is shared by reference with derived gates, so a path added later
   still reaches sessions derived earlier. The instruction loader takes
   no gate, so it consults a process-wide copy instead
   (`childenv.WithholdFiles`, the file-shaped twin of `Withhold`).
   `FromConfig` fills that copy as well, so a library host gets the
   `@include` refusal too. `run()` also fills it directly after
   `withholdDaemonCredentials`, so it does not depend on the gate being
   built before the boot-time instruction load. Process-wide rather than
   an option threaded through seven `instruction.Load*` call sites: a
   call site that forgot the option would be the #647 shape again. The
   refusal is fatal, like any bad `@include`. An `AGENTS.md` that
   includes the table fails the boot and every `POST /sessions` with
   `instruction: <path> is one of the daemon's credential files and is
   never loaded into a prompt`. This adds no new denial of service: an
   agent that can write `AGENTS.md` could already break it with an
   include of a file that does not exist.
1a. **A match is the same file now, not the file seen at boot.** Every
   check stats the configured path afresh and compares inodes. A
   remembered inode was tried and removed: once the file is replaced
   (every Secret update) the kernel reuses the number, and the first
   draft refused an unrelated `AGENTS.md` in this package's own test
   run. The cost is one extra `stat` per credential file per checked
   path, which a `grep` walk pays per file.
2. **The table is protected whenever it is configured, not only when
   multi-session is enabled.** A table that is switched off still holds
   live tokens. The boot that turns multi-session on should not be the
   first boot that protects it.
3. **The set is a hand-written list of caller credentials, not a
   reflective walk.** Unlike `*_env`, "is a file path" is not something
   a field's name tells you. `system_prompt_file`, `peer_state_file` and
   `instructions_file` are paths but not credentials. `attach.tls_key`
   is excluded: it authenticates the daemon to its clients, not a caller
   to the daemon, so it answers no prompt. Provider and cloud
   credential files are excluded for the same reason #1157 excluded
   their env vars. Item 3's daemon-side `--attach-token-file` belongs in
   this set when it lands.
4. **The refusal cannot be approved.** An out-of-scope read escalates to
   an operator. This one does not, because approving it would mean
   answering a prompt with the credential that answers prompts. In
   yolo, allow mode or under an allow-always grant there is nobody to
   ask anyway. It runs ahead of the control-plane write tier, which can
   still be approved.
5. **The bash check is a seatbelt and its tests say so.**
   `TestCredentialBashCheckIsASeatbeltNotABoundary` pins three evasions
   that pass. If one of them starts being refused, update this table
   rather than delete the case. The check matches whole shell words, so
   `testdata/users.json` and `users.json.bak` are not false positives
   for a table at `./users.json`. Hooks reach the gate through
   `CheckBash` too, so a hook command that names the table is refused.
   That is deliberate, and it is a behaviour change. A hook is a shell
   command the daemon runs as its own user, so a hook that reads the
   table is the same route with a different trigger.
6. **The daemon does not try to take the file away from its own user.**
   (b) in the brief was "read at boot into memory and, if configured,
   drop the agent's ability to reach it". The read-once half already
   held. `LoadUsersFile` runs at boot, in multi-session wiring and in the
   startup summary, and is never re-read. Every in-process way to drop
   reach was rejected:
   - **unlink after load** breaks the next restart and fails on a
     read-only Secret volume;
   - **chmod 000** is reversible by the same user, which is the agent;
   - **Landlock** grants access by allowlist. Denying one file means
     allowlisting everything else, and the daemon would restrict itself
     as well as the agent;
   - **setuid after load** would change the daemon's identity for every
     other purpose.

   A real boundary has to come from the operating system, outside
   core-agent. Either `bash` is disabled on a multi-session daemon, or
   the deployment arranges that the daemon's user cannot read the file
   once the daemon has loaded it. The startup warning says this.
7. **Item 5 warns; it does not refuse.** Refusing would turn an upgrade
   into an outage for every privileged deployment, including ones with
   reasons to be privileged. It would also contradict #1157, where a
   failed `Protect` is a warning and not a refusal. The daemon cannot
   tell from inside whether something else isolates the agent, such as
   a sandboxed shell or a user namespace it does not share. The warning
   names the fix: run as non-root, or drop `SYS_PTRACE`.
8. **Item 5 reads the bounding and inheritable sets for root, and the
   ambient set for anyone.** This follows capabilities(7). If the
   parent's real *or* effective uid is 0 at `execve`, the new program is
   treated as having every file capability, so the child's permitted
   set is the parent's inheritable set, OR its bounding set, OR its
   ambient set. For any other parent, only the ambient set passes
   through. A non-root daemon's own effective set never reaches its
   child, so it is not read. The bounding set comes from
   `prctl(PR_CAPBSET_READ)`, the inheritable set from `capget(2)`, and
   the ambient set from `PR_CAP_AMBIENT_IS_SET`; an error from the last
   means a kernel older than 4.3, with no ambient set.
   `TestReadPtraceCapsUnprivileged` cross-checks all three against
   `CapBnd`, `CapInh` and `CapAmb` in `/proc/self/status`, so a wrong
   option or capability number cannot pass silently. If a root daemon's
   capabilities cannot be read, the warning fires in its "could not
   read" form.
9. **The Scion image is documented, not changed.**
   `extras/scion/Dockerfile` ends as root on purpose: Scion's runtime
   picks the container user with `docker run --user`, mirroring the
   Antigravity harness. Docker's default capability set already drops
   `CAP_SYS_PTRACE`, from the bounding set as well. The risk is a
   privileged or `--cap-add SYS_PTRACE` run, which the new warning now
   reports. The Dockerfile comment and the Scion reference page say so.
   The release image is already `nonroot`.

## Out of scope

- **Dropping `CAP_SYS_PTRACE` from the daemon's own bounding set.**
  `PR_CAPBSET_DROP` acts per thread, so it needs
  `syscall.AllThreadsSyscall`. That call is unavailable when cgo is on.
  The inheritable and ambient sets also have to be cleared, or a root
  child gets the capability back. This is worth a follow-up with its
  own tests; it is not part of a warning.
- **A sandboxed `bash`** (Landlock, a mount namespace, or a separate
  uid applied only to the child) would make the bash route enforceable.
  It needs a re-exec shim, because Go cannot run code between `fork`
  and `exec`. It is a design of its own.
- **Passing the table on an inherited fd or a pipe**, so it never sits
  on disk. `LoadUsersFile`'s mode check expects a regular file.
- **Tools outside the built-in suite**, meaning MCP servers and skill
  scripts. They are gated per call, not per path. An MCP filesystem
  server can read the table, and the startup warning's in-scope arm
  points at that. Skill resources are read through `os.DirFS`, which
  follows links, without the gate. Planting a link in a skill
  directory takes `bash`, though, and an agent with `bash` is already
  in the warned class.
- **Items 2 and 3 of #1201**, which are separate work.
