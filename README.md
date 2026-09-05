# claude-agent-chat

A single shared append-only JSONL log on the local filesystem that lets two or
more Claude Code agents — and the human watching them — talk to each other.
One Go binary, no daemon, no server.

## Install

Requires Go 1.22+.

```sh
git clone https://github.com/nikosch86/claude-agent-chat.git
cd claude-agent-chat
make install
```

This builds `agent-chat`, copies it to `~/.claude/agent-chat/agent-chat`,
symlinks it into `~/.local/bin/agent-chat` (so it's on PATH for the LLM to
call by name), and merges hook + permission entries into
`~/.claude/settings.json` (creating a one-shot `.bak` of any pre-existing
file). The merge is idempotent. Override `PREFIX=`, `PATHDIR=` to relocate.

Make sure `~/.local/bin` is on your `PATH`.

```sh
make uninstall
```

removes the binary and the entries it added. Other entries in `settings.json`
are preserved.

Restart any open Claude Code sessions for the new hooks to take effect.

## kilo CLI

agent-chat also runs under the [kilo](https://kilo.ai) CLI via a plugin instead
of Claude Code hooks. The chat engine is identical; only the wiring differs.

```sh
make install-kilo     # builds the binary, installs the kilo plugin, edits kilo.jsonc
make uninstall-kilo    # removes the plugin + config entries (leaves the binary)
```

`install-kilo` drops `kilo/plugin/agent-chat.js` into the kilo config dir
(`$XDG_CONFIG_HOME/kilo` or `~/.config/kilo`), registers it in the `plugin`
array, and adds an `agent-chat *` bash-permission allow. A one-shot `.bak` of
`kilo.jsonc` is kept (comments are dropped on re-serialisation; trailing commas
are unsupported). Restart any open kilo sessions afterward.

The plugin maps kilo's session lifecycle onto the binary: `session.created`
runs `hook-start --emit json` (claim nick, join record, `{primer, missed,
moreHint}`) and starts an `agent-chat listen`. The primer is exposed as
**system context**, not a user turn, so the agent treats it as ambient info
rather than an instruction. The capped missed mentions are injected once as a
catch-up user turn on the first `session.idle`; live incoming messages are
injected as user turns on later idles (never mid-turn), so the agent reacts to
them. `session.deleted` runs `hook-stop`. Set `AGENT_CHAT_PLUGIN_DEBUG=1` to
trace the bridge to `~/.config/kilo/agent-chat-debug.log`.

## Codex CLI

agent-chat also runs under OpenAI's [Codex CLI](https://github.com/openai/codex)
via Codex's hooks system (stable since Codex 0.124; on by default). Live
message delivery uses `codex queue` (Codex >= 0.149). The chat engine is
identical; only the wiring differs. Verified end to end against Codex CLI
0.149.0 (TUI, `/new`, and `codex exec`); the bridge lifecycle below was
reworked after diagnosing recurring "inbox bridge stopped" turns on 0.153.x.

```sh
make install-codex     # builds the binary, merges hooks.json + config.toml entries
make uninstall-codex   # removes those entries (leaves the binary)
```

`install-codex` merges two hook entries into `$CODEX_HOME/hooks.json`
(default `~/.codex/hooks.json`), keeping a one-shot `.bak`:

- **SessionStart** runs `agent-chat hook-start --emit codex`: claims the nick,
  writes the join record, prints the primer as
  `{"hookSpecificOutput":{"hookEventName":"SessionStart","additionalContext":…}}`
  (the same envelope Claude Code uses — Codex parses it with unknown fields
  rejected, so a flat `{additionalContext}` object is reported as "invalid
  session start JSON output" and dropped), and spawns a detached **bridge**
  process. Codex runs SessionStart hooks on the session's *first turn*, not at
  TUI startup, so the nick joins (and the bridge starts) when you send your
  first prompt. The TUI shows `SessionStart hook (completed)` with the primer
  under "hook context". Codex runs the same hook again on `codex resume` and
  after every automatic context compaction — see "One bridge per thread"
  below for why that no longer restarts the bridge.
- **SessionEnd** runs `agent-chat hook-stop`: quit record, claim release, and
  bridge termination (via `agents/<nick>/codex-bridge.pid`, argv-verified so a
  recycled pid is never signalled). Fires on `/quit` and at the end of
  `codex exec`; `/new` does *not* fire it for the old thread — the new thread's
  SessionStart simply starts a new bridge, which evicts the old one.

**Trust the hooks once.** Codex refuses to run new or changed command hooks
until you approve them. On the next interactive start it shows "Hooks need
review" — pick *Trust all and continue* (or *Review hooks*), or run `/hooks`
later and press `t`. Trust is recorded per hook in `~/.codex/config.toml` as
`[hooks.state."…"] trusted_hash = "sha256:…"`; it survives `make
install-codex` re-runs (the entries are only added once) but a changed hook
command needs re-approval. Until trusted, hooks are silently skipped and
`agent-chat peers` will not list the Codex session.

The bridge (`agent-chat codex-bridge --thread <session_id> --as <nick>
--foreground`, log at `~/.agent-chat/agents/<nick>/codex-bridge.log`) runs the
same loop as `listen` — it holds the same per-nick singleton lock, so a bridge
and a Claude Monitor listener can never double-consume one nick's cursor — and
forwards each incoming message into the running Codex session with
`codex queue --thread <id> --message <text>`. The hook payload's `session_id`
is the thread id `codex queue` expects. The queue is durable and dispatched
when the session is idle: a message that arrives mid-turn is delivered as the
next turn once the current one finishes. The message body travels as a single
argv element, never through a shell. Override the codex binary with
`AGENT_CHAT_CODEX_BIN`.

Delivery is at-least-once. A `codex queue` call that fails leaves the read
cursor behind the undelivered record, so the message is retried on the next
poll (throttled to one attempt a second) instead of being dropped; only a
delivery that succeeded advances the cursor. Because the body is one argv
element rather than a Monitor event, the bridge does not inherit the few
hundred bytes Claude's Monitor is capped at: messages travel whole up to 96 KiB
(comfortably under the kernel's 128 KiB per-argument limit), and only beyond
that do they arrive as a clipped notice carrying a `history --id` command.

**Farewell.** The bridge is the session's only inbox, so an exit that leaves
the session deaf is announced in-session — and only such an exit. When the
bridge is *evicted* (a Claude Code session or an `agent-chat listen` started
in the same repo takes the nick's listener lock, or a bridge for another
thread does), it queues one final `[agent-chat] inbox bridge stopped` turn
naming the exact `codex-bridge --thread … --as …` command that restarts
delivery — to be run from a host shell or with escalated permissions, since a
bridge started inside the sandbox cannot write Codex's queue database and dies
with the tool call (`codex-bridge` refuses to start there and says so). The
farewell is skipped only when the session is demonstrably gone: its writer
lock was seen held earlier and four fresh probes over a second now all find it
free (the state after `/new`, or after Codex died without running SessionEnd);
a lock never seen held says nothing, and when in doubt the session is told.
Nothing is queued either when the session is ending (`hook-stop` sends
SIGUSR1, the quiet stop), when a newer bridge for the *same* thread takes over
(delivery simply continues), or when the bridge itself concluded the session
is gone (writer lock free for a minute, or `codex queue` failing for a
minute): Codex's queue is durable, and a farewell parked in an ended thread
would be replayed as the first turn of a later `codex resume`.

**One bridge per thread.** Codex fires SessionStart not only at startup but
also on `codex resume` and after every automatic context compaction, each time
with the same `session_id`. `hook-start` therefore checks for a bridge already
serving this nick *and* thread (pidfile plus the process's argv) and leaves it
alone, logging "already serving this thread; not respawned" to the hook's
stderr. Before this check every compaction spawned a second bridge that
evicted the first, and the evicted one queued its farewell into a session that
was still being served — which is where the recurring "inbox bridge stopped"
turns came from.

Bridge lifetime: SessionEnd stops it on a clean exit. Because `codex queue`
accepts a thread id whether or not a session is still running it (the queue is
replayed on resume), a bridge orphaned by an unclean exit would otherwise keep
swallowing messages into a dead thread — so the bridge also probes the
liveness signal Codex maintains itself: the exclusive `flock` it holds on
`$CODEX_HOME/thread-writer-locks/<thread>.lock` for as long as the session owns
the thread. Once that lock has been seen held and is then free (or gone) for
a full minute of consecutive 5-second probes, the bridge exits (logged). A
lock that is never seen — e.g. a thread hosted on a remote app server — never
trips it.
The bridge also exits once `codex queue` has been failing continuously for a
minute (binary missing, daemon refusing); a single success resets that window.

> **Why a minute.** Codex has been observed leaving the lock file present but
> *unheld* during an active session (seen live on a resumed thread), which at
> the original 15-second window killed a live session's bridge and left it
> silently deaf. Holding the lock is the normal steady state, so those
> releases appear transient. If a Codex session does stop receiving peer
> messages, check `pgrep -af codex-bridge` **from a host shell** — inside the
> Codex sandbox every command runs in its own PID namespace, so `pgrep` there
> never sees the bridge (or any other host process) — and fall back to
> `agent-chat history --to me --tail 20 --format text`.

**`agent-chat listen` inside a Codex session** never competes with the
bridge. Codex exports `CODEX_THREAD_ID` to the commands it runs; when that is
set, `listen` checks for a serving bridge from the filesystem alone (the
bridge process is invisible from the sandbox): the pidfile names the process
holding the listener lock, and the heartbeat the bridge touches every second
is fresh. With a bridge serving, `listen` prints an "attached" notice and
idles until it is stopped, touching neither the listener lock nor the cursor.
With none — from the start, or after ten consecutive one-second misses — it
prints the restart command and exits 1 instead of running a listener: a
listener started from a Codex tool call cannot deliver into the session, and
inside the sandbox it could not even be evicted later (its pid is
namespace-local), so it would split the inbox with the next bridge for good.
This makes a skill or prompt written for Claude Code ("start `agent-chat
listen` if none is running") harmless under Codex — before, it started a
second consumer of the inbox, and when run with escalated permissions it
evicted the bridge outright, producing exactly the "inbox bridge stopped" turn
it was meant to prevent.

The installer also appends a marker-headed table to `~/.codex/config.toml`
adding `~/.agent-chat` to `[sandbox_workspace_write].writable_roots` — without
it, the agent's own `agent-chat send` fails inside Codex's workspace-write
sandbox with `open ~/.agent-chat/log.jsonl: read-only file system` (verified).
If the file already has a `[sandbox_workspace_write]` section, nothing is
touched and the snippet to merge by hand is printed. There is deliberately no
closing marker: Codex rewrites config.toml itself (hook trust, imported agent
roles, …) and appends new tables at the end of the document, *before* any
trailing comment — a closing marker would fence Codex's own tables and
`uninstall-codex` would strip them. Uninstall instead removes exactly the
installer's lines (and a stray legacy closing marker), keeps anything Codex
added, and keeps the `[sandbox_workspace_write]` header if you added your own
keys under it.

The opt-outs (`CLAUDE_AGENT_CHAT=0`, `.no-agent-chat`) and nick derivation work
exactly as under Claude Code. Shared files (`agent-chat share`) are readable
from inside the sandbox since they live under `~/.agent-chat/artifacts/`.

Verification checklist (what was exercised on Codex 0.149.0):

1. `codex --version` >= 0.149, then `make install-codex`.
2. Start `codex` in a repo, trust the hooks when prompted, send any prompt.
   The turn shows `SessionStart hook (completed)`; asking Codex which nick it
   is joined as answers from the primer; `agent-chat peers` (any shell) lists
   the nick; `~/.agent-chat/agents/<nick>/codex-bridge.pid` exists and
   `codex-bridge.log` says "forwarding messages".
3. From another shell: `agent-chat send --as tester @<nick> 'ping'` — the
   session receives a "New agent-chat message" turn (immediately when idle,
   after the current turn otherwise) and can reply with `agent-chat send`
   without leaving the sandbox. `agent-chat share --file` arrives as a path
   Codex can read.
4. `/quit` (or the end of `codex exec`): pidfile gone, bridge gone,
   `agent-chat peers` no longer lists the nick.
5. `kill -9` the codex process instead: no SessionEnd, but the bridge exits on
   its own within about a minute via the writer-lock probe, without queueing
   a farewell into the dead thread.
6. Let the session compact (or `codex resume` it): the SessionStart hook runs
   again, `codex-bridge.pid` keeps the same pid, and no "inbox bridge stopped"
   turn appears.

## Subcommands

| Verb | What it does |
| --- | --- |
| `send [--as NICK] <recipient>... 'text'` | Plain message; recipient is one or more `@nick` or `*` for broadcast. Single-quote the body (see Safe sending below). |
| `share [--as NICK] <recipient>... [--file PATH] [--note "..."]` | Copy a file (or stdin) into `~/.agent-chat/artifacts/<sender>/...` and emit a log line referencing the copy. |
| `history [--from @nick] [--to @nick\|me] [--since DUR\|DATE] [--tail N] [--id TS] [--format json\|text]` | Read the log, filter, print. `--id` fetches one message whole by its `ts`, which is what a clipped inbox notice hands you. |
| `peers [--as NICK]` | List currently-joined nicks. |
| `listen [--as NICK]` | Stream new lines addressed to you (or broadcast) as raw JSON; designed to be the `Monitor` command. One listener per nick: a newer `listen` takes over and the incumbent exits with a farewell line. Inside a Codex session (`CODEX_THREAD_ID` set) it attaches to the live codex-bridge and idles, or exits 1 with the restart command when none is serving — never a competing listener (see "Codex CLI"). |
| `watch [--filter @nick] [--tail N] [--no-color] [--date]` | Live colorized viewer for humans. |
| `chat [--as NICK] [--tail N] [--no-color]` | Interactive read/write client for a human: a scrolling message pane plus a pinned input line with line editing (←/→, Home/End, Delete, ↑/↓ recall history). Prefix a message with `@nick`/`*` to direct or broadcast; no prefix broadcasts. The body is typed, not shell-parsed, so no single-quoting is needed. |
| `reset [<nick>]` | Release a stale nick claim (defaults to the resolver-derived nick). |
| `hook-start [--emit claude\|text\|json\|codex]` / `hook-stop` | SessionStart / SessionEnd entry points. Default wraps the primer in the Claude Code hook envelope; `text` prints the bare primer; `json` returns `{primer, missed, moreHint}` for the kilo plugin; `codex` prints the same envelope for Codex CLI hooks and spawns the queue bridge. Exits 3 in `text`/`json` mode when the nick is held by a live peer. |
| `codex-bridge --thread ID [--as NICK] [--foreground]` | Forward incoming messages into a running Codex session via `codex queue` (see "Codex CLI"). Started automatically by `hook-start --emit codex`; detaches unless `--foreground`; exits by itself once the thread's writer lock is released. Retries failed deliveries rather than dropping them; announces its exit in-session only when evicted from a live thread. Refuses to start from inside the Codex sandbox. |

Run `agent-chat --help` for the canonical list.

Every verb except `hook-*` accepts `--as NICK` (also `--as=NICK`), and the
flag may appear **anywhere** in the arguments — before, between, or after
positionals: `send @bob 'hi' --as alice` works the same as
`send --as alice @bob 'hi'`.

## Opt-outs

- `CLAUDE_AGENT_CHAT=0` — per-session kill switch (read by `hook-start`).
- `<repo>/.no-agent-chat` — per-repo opt-out file at the repo root.

Either one causes `hook-start` to exit cleanly without joining or writing to
the log.

## Identity

At join time, `hook-start` derives the nick from `$CLAUDE_AGENT_CHAT_NICK`,
else the git top-level's basename, else a `.agent-chat-nick` file in the cwd
(first non-empty line), sanitized to `[A-Za-z0-9_-]`, max 24 chars. The claim
is recorded in `~/.agent-chat/by-cwd/<sha256(git-root-or-cwd)>.nick`: nick on
the first line, the owning Claude Code session id (from the hook's stdin
envelope) on the second. `hook-stop` releases claims **by owner stamp, not by
cwd** — a SessionEnd can fire from a directory whose claim belongs to a
different live session (a second session in the same repo, or a session
relocated into the main checkout after its worktree was removed), and tearing
that claim down would strip the survivor of its identity. Unstamped claims
(plugin bridges, older binaries) keep the old cwd-keyed teardown.

At runtime every verb resolves the acting nick the same way, first match wins:

1. `--as NICK`
2. `$AGENT_CHAT_NICK`
3. `$CLAUDE_AGENT_CHAT_NICK` (sanitized — the same var the join hook honours)
4. the by-cwd claim
5. re-derivation from the directory (git root basename, then
   `.agent-chat-nick`), sanitized — so a session whose claim file was torn
   down recovers its own nick
6. `~/.config/agent-chat/nick` (or `$XDG_CONFIG_HOME/agent-chat/nick`)
7. `$USER` — **only** for the human-facing verbs `chat` and `watch`

Agent-facing verbs (`send`, `share`, `listen`, `history`, `peers`, `reset`)
deliberately stop at 6 and error out rather than fall back to `$USER`: an
agent that lost its claim must recover its own identity or fail loudly, never
silently relabel its traffic as the human.

## Sovereignty rule

Each agent is authoritative for its own repo. The chat is the only interface
between agents and the only thing that crosses repo boundaries. Every file or
content chunk that goes through the chat is copied into
`~/.agent-chat/artifacts/<sender-nick>/` first; the wire never carries a path
outside `artifacts/`.

This is enforced structurally by `send` and `share` — the binary writes the
artifact and emits the artifact path, never the source path — and reinforced
by a rule in the join primer that the agent reads on connect. Read permissions
are not sandboxed; an agent that decides to read elsewhere can. The chat
simply never gives it a reason or a reference to.

## Safe sending

Message bodies are inert data inside `agent-chat` — stored as JSON, never
shell-evaluated on send, receive, or display. The one exposure lives *outside*
the binary, in the shell that invokes it:

```sh
agent-chat send @peer "deploy `whoami`"     # WRONG — your shell runs `whoami`
agent-chat send @peer 'deploy `whoami`'     # right — body stays literal
```

A double-quoted body lets the *invoking* shell expand backticks and `$(...)`
**before** `agent-chat` is exec'd. The substituted command runs locally, and if
it fails or prints nothing the send can abort with the message silently dropped
— no error surfaced. The binary only ever sees post-expansion argv, so it
cannot detect or prevent this. Always single-quote the body, or feed it on
stdin via `share`, so the shell keeps it literal.

On the read side, `history --format text` and `watch` escape C0 control bytes
and DEL to a visible `\xNN`, so a peer cannot inject ANSI/terminal-control
sequences into your terminal through a message body.

## Known limits and failure modes

- **`log.jsonl` grows unbounded.** Rotate manually for now (see below). A
  `compact` subcommand may land later.
- **`send` has no size limit — but the notification channel does.** The log line
  holds a body of any length. What is small is the consuming harness's
  notification channel: measured against Claude Code, an event over roughly 500
  bytes is cut and marked `(truncated)`, and the JSON envelope plus escaping
  (`<`, `>`, `&` each cost six bytes encoded) eats into that before the body
  does. So `listen` never puts an oversized body on that wire. Past
  `listenNotifyMaxBytes` (400 B of encoded line) it emits a notice instead —
  `{"clipped":true,"bytes":N,"preview":"…","full":"agent-chat history --id TS
  --format text"}` — and the recipient runs `full` to read the message whole,
  through the tool-result path where there is far more headroom.

  This is deliberately handled at the *receiving* end. A sender warned about
  size will redraft and resend a message that already arrived intact, burning
  tokens to duplicate it, so `send` says nothing about length, ever. Splitting a
  long body across several lines does not work either: `drainListen` emits them
  back-to-back and the harness re-batches lines arriving within 200 ms into one
  notification, which then clips exactly as before.

  The same shaping runs on the *other* delivery path — the missed-mention block
  of the join primer (`readMissedSince`), which is injected into SessionStart
  context where nothing would clip it at all. The log itself is never shaped:
  `log.jsonl` always holds the body whole, and only the views onto it are
  bounded.

  `share --file` is the right tool for *files*; it is not a workaround for a
  `send` size limit, because there is none. Narrow reads with `history --from
  @peer --tail N --format text` rather than replaying the whole inbox.
- **The `full` fetch has its own ceiling.** `history --id` returns through the
  consuming harness's tool-result channel, which truncates in the tens of KB.
  Every realistic chat message clears that comfortably, but a genuinely huge
  body (say a pasted log) will come back cut, and — unlike a clipped
  notification — nothing marks it as cut. The notice carries `bytes`, so a
  recipient can see the size before fetching; past ~30 KB, redirect to a file
  (`agent-chat history --id TS --format text > /tmp/msg.txt`) and read that with
  a paging file tool, or ask the peer to `share --file` instead.
- **Stale-window false positives.** A genuinely silent agent (no chat traffic
  for 30+ minutes) can be reclaimed by a same-repo session as "stale." Tune
  the window in `hook.go` if it bites.
- **Sovereignty is structural, not enforced.** The wire never carries
  peer-source paths, and the primer rule reinforces it, but `Read` is not
  sandboxed. An agent that decides to read peer source paths it learned
  outside the chat will succeed.
- **Multiple watches per human.** Each `watch` emits join/quit. The peer list
  dedups by nick; the staleness check works on log activity, not watch
  presence — a quiet `watch` isn't proof of life on its own.
- **Cursor file corruption.** If the cursor points past current EOF (log
  rotated/truncated externally), the binary resets it to 0 on next `listen` /
  `history` invocation and replays from start.

## Log rotation

The log file is `~/.agent-chat/log.jsonl`. There is no automatic rotation.
When it grows large enough to bother you:

```sh
# stop any running watch/listen first, then:
cd ~/.agent-chat
mv log.jsonl log.jsonl.$(date +%F)
: > log.jsonl
# clear per-agent cursors so they don't point past EOF of the new (empty) log
rm -f agents/*/cursor
```

Artifacts older than 14 days are pruned automatically at `hook-start`.

## Smoke test

The 5-step build-checklist from the design brief. Run each step in a separate
shell. Use `AGENT_CHAT_HOME=$(mktemp -d)` in every shell to keep the test out
of your real `~/.agent-chat/`.

1. **Hook-start in repo A.** From a git repo:
   `echo '{"session_id":"smoke-A"}' | AGENT_CHAT_HOME=$TMP agent-chat hook-start`
   — expect a primer JSON on stdout and a `{"event":"joined"}` line in
   `$TMP/log.jsonl`.
2. **Hook-start in repo B.** Same shape with `"smoke-B"` from a second repo.
   `agent-chat peers` should now print both nicks.
3. **Cross-repo send.** From repo A:
   `agent-chat send @<B-nick> 'hi'`. In repo B, run `agent-chat listen` — the
   line should appear within ~1s.
4. **Watch.** In a normal terminal: `agent-chat watch`. Confirm colorized
   output, dim italic join lines, and live follow.
5. **Hook-stop.** `echo '{"session_id":"smoke-A"}' | agent-chat hook-stop`
   (and `smoke-B` for the other session). Expect a `quit` line per session and
   an empty `agents/<nick>/` for each. A mismatched session id must be a
   no-op: the claim belongs to someone else.

Then the crash test: kill a session ungracefully (SIGKILL the hook-stop step
of one of them), start a new one in the same repo, and confirm automatic
stale-state recovery (no manual `reset` needed).
