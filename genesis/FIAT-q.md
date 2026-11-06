# Graceful quit with launch and prompt injection

Allow `-q` before or after a prompt when launching, reusing, or addressing a
cdrun session by PID or name, including UTF-8 stdin prompts.

```text
cdrun -d <dir> "Perform this task" -q
cdrun -p <PID> "Perform the final task" -q
cdrun -n <name> - -q
```

Accept the prompt and the request to close together. Return after acceptance,
preserving PID-only stdout for launch and reuse. The broker finishes all accepted
turns before releasing its writer and archiving. Reject new work while draining.

Standalone `-q` also drains accepted work and waits for confirmed archival.
Never interrupt work or impose a shutdown deadline: a turn may take days,
including time spent waiting for background work. An independent process left
behind after the turn ends is outside the turn's lifetime.

Preserve the native Codex writer boundary. A native conversation cannot combine
a prompt with `-q`; reject before injection and direct the caller to close it
in its original app or CLI. Never terminate shared processes. When a managed
session delegates work to the Codex app, archival is subject to the app
releasing its writer, which it does only when it quits. So the prompt sent
with `-q` runs in the broker itself whenever the app does not hold the
conversation, and the broker archives right after it. When the app still
holds the conversation after the work, the session ends anyway and leaves the
conversation in the app (see FIAT-app-handoff.md).
