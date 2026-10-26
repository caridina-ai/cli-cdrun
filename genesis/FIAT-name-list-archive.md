# Named sessions, repeatable launches, and confirmed archival

Date: 2026-10-19

## Requirements

Use one caller-controlled name for creation and later selection. Add exact,
case-insensitive `-n` selection and `-l [keyword ...]` discovery. Numeric names
are valid; `-p` remains exclusively a positive broker PID. Remove the old `-r`
option without an alias. Match the common usage forms and their order with
ccrun v0.2.0, retaining documented cdrun extensions.

A repeated `-d DIR [-n NAME] [prompt]` reuses one matching running session only
when its directory also matches. Otherwise start a new named session, or reject
ambiguous names and directory conflicts before creating the requested directory.
Successful launches and reuses print only their PID on stdout. Default the name
to the directory basename. All failures exit 1 and all successes exit 0.

List only sessions cdrun can control, verifying the process birth time, full
executable path, and authenticated broker endpoint. Match every keyword against
the name, directory, or thread ID. Put the header on stderr and PID-first rows
on stdout, with display-width padding for Chinese text.

`-q` must confirm archive completion before reporting success.

## Codex name investigation

| Candidate | Caller can set at creation | Locally readable | Stable after resume | App and paired phone title |
|---|---|---|---|---|
| User-facing thread name (`thread/name/set`) | Yes, immediately after `thread/start` | Yes: `thread/read`, `thread/resume`, `thread/list`, local session index | Yes, persisted separately from process identity | Yes, the thread's user-facing name |
| Thread ID | No, allocated by Codex | Yes | Yes | Identifier, not display title |
| Generated preview | No | Yes | Content-derived, unsuitable as a caller-controlled name | May supply a fallback title |
| Broker PID | No, allocated by Windows | Yes | No, changes with a new broker | No |

Decision: `-n` uses the persisted user-facing thread name. Naming succeeds before
publishing the broker endpoint, so immediate name selection has no startup gap.
The broker retains that name while its private app-server stops and resumes the
thread. A new launch after broker termination creates a new thread with the same
requested name. Native Codex app/CLI processes are not cdrun brokers and cannot
be addressed by their OS PID through this interface.

Reference: [official Codex app-server protocol](https://learn.chatgpt.com/docs/app-server),
particularly `thread/name/set`, hydrated thread names on read/resume/list, and
`thread/archive`. Paired-phone rendering remains a human acceptance check.

## Implementation decisions

- Serialize same-name discovery and launch with a Windows mutex. Pin its owning
  Go goroutine to the OS thread, as required by Windows mutex ownership.
- Verify live broker identity rather than accepting stale registry JSON.
- Use Windows path cleaning, separator normalization, and case-insensitive
  directory comparisons; inspect conflicts before creating the working directory.
- Keep the name and listing interface independent of downstream integrations.

## Archive investigation and repair

The previous quit client waited ten seconds, requested forced closure, then
waited only five seconds before terminating the broker. An archive RPC can wait
sixty seconds, so this could kill the broker during archival. Archive connection
errors were discarded, RPC errors were only saved in the final snapshot, and
the quit client returned success without checking that snapshot. Shutdown could
also race the worker and leave its app-server holding the thread.

Shutdown now waits for the worker to stop, releases its own writer, and attempts
archival up to three times using fresh app-servers. The broker records
`Archived=true` only after Codex acknowledges success. The quit client holds a
verified process handle, waits for broker exit, and checks the final snapshot.
Archive failure, missing confirmation, or timeout returns failure; the client
never kills a broker merely because archival takes longer than five seconds.
The ten-second work grace remains, followed by a bounded wait for shutdown and
archival. The maximum client wait is eight minutes. An external writer's refusal
is reported explicitly instead of being treated as successful archival.

## Release gate

Prepare a 0.2.0 release. Human acceptance is required before committing,
tagging, pushing, and verifying installation of the published version.
