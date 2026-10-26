# Discover and address native Codex conversations

Date: 2026-10-20

## Correction to the original scope

Listing only cdrun's own brokers missed the user's expectation: a conversation
opened by Codex app or CLI must also be discoverable and addressable. This
supersedes the native-process exclusion in the preceding naming decision.

## Evidence and implementation

The installed Codex maintains per-thread files under
`CODEX_HOME/thread-writer-locks`. A held Windows byte-range lock identifies an
active writer; file existence alone is not sufficient. Read metadata using
`thread/read` without resuming or renaming the conversation. A separate client
reports `notLoaded` even for threads held by another server, so its loaded-thread
list cannot serve as machine-wide discovery.

A shared app-server PID cannot uniquely identify its conversations. Give each
discovered native thread a private observer process and expose that observer's
real PID in the existing interface. Keep the thread ID internal to routing.
Observers use authenticated local endpoints and the same process-birth and
executable-path verification as owned brokers. Discovery and owned startup share
a registry mutex to prevent duplicate proxies during endpoint publication.

The observer uses `thread/queue/add` to inject into the original writer, and
`thread/read`, `thread/turns/list`, and `thread/queue/list` for saved status.
It never resumes or takes over a native conversation. Names follow native
metadata, including renames. Unnamed conversations use their preview or directory
name for selection. Observers exit after the writer disappears; resume can
therefore yield a new proxy PID under the same persisted name.

`open` means a writer holds the conversation, not necessarily that the model is
generating. Saved turn data can lag the app. Native JSON identifies itself with
`Native=true`; sequence and completed counts refer to the returned recent page.
Unknown queue length is -1. All discovery is scoped to the current Codex home.

## Closing native conversations

An external writer can refuse `thread/archive`. Report that failure, retain the
observer while the writer is present, and never kill the shared app-server.
Native cancellation must be performed in its owning app/CLI. This limitation is
explicitly documented rather than reported as successful closure.

## Validation

Use isolated private app-server fixtures as native owners, not existing user
conversations. Verify discovery, exact name and PID/stdin injection, queue
contents, repeated directory/name reuse, rename, writer release, resume with a
new observer PID, and explicit archive refusal without terminating the owner.
Unit tests cover held versus stale writer locks and registry-lock reentrancy.
The current desktop conversation was additionally discovered and read without
injecting into or archiving it.
