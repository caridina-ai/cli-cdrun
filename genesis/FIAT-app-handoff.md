# Run turns in the Codex app so desktop and mobile stay live

Date: 2026-11-06

## Problem

A cdrun turn ran in the broker's private app-server, which held the thread's
writer lock for the whole turn, including a long `sleep`. The Codex desktop app
and the phone paired with it then reported that another program was using the
conversation. A conversation opened in the desktop app has no such limit: its
windows and the paired phone are all clients of the app's own app-server, so
they can watch it and keep typing into it.

## Rejected approach

An opt-in loopback WebSocket app-server (`CDRUN_APP_SERVER_URL`) required
starting a separate server and relaunching the desktop app against it through
an internal environment variable. It was removed, together with its
WebSocket dependency.

## Evidence

Inspected Codex desktop 26.1023.6123.0 and codex-cli 0.165.0-alpha.1.2:

- On Windows the desktop app owns a stdio app-server; its shared local daemon
  path is disabled there, so no external client can attach to it.
- `thread/queue/add` persists a prompt in the thread's queue across processes;
  `codex queue --thread <id> --message <text>` does the same.
- The app's `codex://threads/wake/<thread>` link resumes that thread in the
  app's own app-server when its queue is non-empty, then runs the queue. Wake
  links never navigate or show a window; a wake-only launch starts the app in
  the background.
- While the app holds a thread it also runs prompts queued later, without
  another wake. A non-writer app-server can follow turns with
  `thread/turns/list`; the user message carries the `clientUserMessageId` as
  `clientId`. An unfinished turn read by a non-writer has `completedAt=null`.
- A protocol-activated `ChatGPT.exe` is a direct child of the caller, so it
  would join a broker's kill-on-close Job Object.
- Codex refuses `thread/archive` while another app-server holds the thread,
  and so does `codex archive`. No app link, queue entry or app-server request
  asks the app to let go or to archive. A thread archived elsewhere has its
  rollout under `archived_sessions`.
- The app keeps a conversation it ran after a wake open until it quits: its
  idle release only tracks conversations opened in a window, three hours
  after they were left, and never while a phone follows them. A woken probe
  conversation that was never opened stayed held for more than a day.

Verified end to end: a launched session's 40-second sleep ran in the desktop
app, the broker reported the app's turn and answer, and its private app-server
never ran a turn.

## Decisions

- Default: queue each prompt, wake the app when no writer holds the thread,
  and follow the turn by its client message ID until `completedAt` is set. A
  turn whose writer disappears before completion is reported as interrupted,
  never replayed.
- When no writer takes the prompt within 60 seconds, or the wake fails,
  withdraw it with `thread/queue/delete` and run it in the private
  app-server. A failed withdrawal means a writer took it; keep following.
  `CDRUN_CODEX_APP=0` always runs prompts privately, as before.
- The prompt sent with `-q` runs in the private app-server whenever no other
  writer holds the conversation, and never wakes the app, so the broker
  archives right after it. Such prompts are batch work, e.g. a scheduled
  `cdrun -d <dir> "<task>" -q`, which must finish and close; following them
  elsewhere does not matter. Interactive sessions start without `-q` and keep
  every prompt in the app. If the app already holds the conversation, the
  final prompt joins the app's queue and the conversation stays in the app.
- Open the wake link from a short-lived helper (`cdrun __wake`) started with
  `CREATE_BREAKAWAY_FROM_JOB`. The broker's job allows breakaway only while
  that helper starts. If an enclosing job forbids it, the prompt runs
  privately. Use `SEE_MASK_FLAG_NO_UI`, so machines without the app show no
  dialog.
- Accepted work always finishes first, however long it takes. Archival then
  lets the app run prompts still queued in Codex, and waits 15 seconds at
  most for another writer to let go: the app would keep the conversation
  until it quits, and an interactive session closed with standalone `-q`
  must not hang. If a writer still holds it, the broker ends with
  `LeftOpen`, leaving the conversation to be archived in the app; standalone
  `-q` says so and exits 0. Archival done in the app counts as success. The
  app's processes are never killed. While archiving, the broker reports
  `archiving`, accepts no prompts, and no longer claims its name for `-d -n`
  or `-n`.
- `--cancel` interrupts only turns the broker runs itself.
- Turns run by the app use the app's environment and approvals.

## Human acceptance

Start `cdrun -d <dir> -n <name>`, then send it a prompt that sleeps 120
seconds and replies DONE. During the sleep, open the conversation in the
desktop app and on the phone; both must show live progress and accept input.
Then `cdrun -p <PID> -q` must return within seconds of the work ending,
saying the conversation is still open in the app.

Run `cdrun -d <dir> -n <name> "<task>" -q` as a scheduled batch job: it
must exit at once with the PID, and the conversation must be archived as
soon as the task ends.
