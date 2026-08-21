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
identical; only the wiring differs.

```sh
make install-codex     # builds the binary, merges hooks.json + config.toml entries
make uninstall-codex   # removes those entries (leaves the binary)
```

`install-codex` merges two hook entries into `$CODEX_HOME/hooks.json`
(default `~/.codex/hooks.json`), keeping a one-shot `.bak`:

- **SessionStart** runs `agent-chat hook-start --emit codex`: claims the nick,
  writes the join record, prints `{"additionalContext": "<primer>"}` (the flat
  control object Codex hooks consume — no `hookSpecificOutput` envelope), and
  spawns a detached **bridge** process.
- **SessionEnd** runs `agent-chat hook-stop`: quit record, claim release, and
  bridge termination (via `agents/<nick>/codex-bridge.pid`, argv-verified so a
  recycled pid is never signalled).

The bridge (`agent-chat codex-bridge --thread <session_id> --as <nick>
--foreground`, log at `~/.agent-chat/agents/<nick>/codex-bridge.log`) runs the
same loop as `listen` — it holds the same per-nick singleton lock, so a bridge
and a Claude Monitor listener can never double-consume one nick's cursor — and
forwards each incoming message into the running Codex session with
`codex queue --thread <id> --message <text>` (durable queue, dispatched when
the session is idle; reaches TUI sessions too). The message body travels as a
single argv element, never through a shell. After 5 consecutive `codex queue`
failures the bridge assumes the session is gone and exits. Override the codex
binary with `AGENT_CHAT_CODEX_BIN`.

The installer also appends a marker-delimited block to `~/.codex/config.toml`
adding `~/.agent-chat` to `[sandbox_workspace_write].writable_roots` — without
it, the agent's own `agent-chat send` would be blocked by Codex's
workspace-write sandbox. If the file already has a `[sandbox_workspace_write]`
section, nothing is touched and the snippet to merge by hand is printed.

The opt-outs (`CLAUDE_AGENT_CHAT=0`, `.no-agent-chat`) and nick derivation work
exactly as under Claude Code. Codex asks the user to review/approve new
command hooks once on the next start — approve both entries.

**Status: prepared but not yet exercised against a live Codex install.**
Verify on a machine with Codex before trusting it:

1. `codex --version` >= 0.149, then `make install-codex`.
2. Start a Codex session in a repo; the primer should be visible as session
   context, and `agent-chat peers` (from any shell) should list the nick.
3. Check `~/.agent-chat/agents/<nick>/codex-bridge.pid` exists and
   `codex-bridge.log` shows "forwarding messages".
4. From another shell: `agent-chat send --as tester @<nick> 'ping'` — the
   Codex session should receive a "New agent-chat message" turn when idle.
   This is the key assumption to confirm: the hook payload's `session_id` is
   accepted by `codex queue --thread`. If queueing fails (see the bridge log),
   the id scheme differs and the bridge needs the real thread id instead.
5. Reply from inside Codex with `agent-chat send ...` — if the sandbox blocks
   it, the `writable_roots` entry (step printed by the installer) is missing.
6. End the session; the pidfile should be gone and `agent-chat peers` should
   no longer list the nick.

## Subcommands

| Verb | What it does |
| --- | --- |
| `send [--as NICK] <recipient>... 'text'` | Plain message; recipient is one or more `@nick` or `*` for broadcast. Single-quote the body (see Safe sending below). |
| `share [--as NICK] <recipient>... [--file PATH] [--note "..."]` | Copy a file (or stdin) into `~/.agent-chat/artifacts/<sender>/...` and emit a log line referencing the copy. |
| `history [--from @nick] [--to @nick\|me] [--since DUR\|DATE] [--tail N] [--id TS] [--format json\|text]` | Read the log, filter, print. `--id` fetches one message whole by its `ts`, which is what a clipped inbox notice hands you. |
| `peers [--as NICK]` | List currently-joined nicks. |
| `listen [--as NICK]` | Stream new lines addressed to you (or broadcast) as raw JSON; designed to be the `Monitor` command. One listener per nick: a newer `listen` takes over and the incumbent exits with a farewell line. |
| `watch [--filter @nick] [--tail N] [--no-color] [--date]` | Live colorized viewer for humans. |
| `chat [--as NICK] [--tail N] [--no-color]` | Interactive read/write client for a human: a scrolling message pane plus a pinned input line with line editing (←/→, Home/End, Delete, ↑/↓ recall history). Prefix a message with `@nick`/`*` to direct or broadcast; no prefix broadcasts. The body is typed, not shell-parsed, so no single-quoting is needed. |
| `reset [<nick>]` | Release a stale nick claim (defaults to the resolver-derived nick). |
| `hook-start [--emit claude\|text\|json\|codex]` / `hook-stop` | SessionStart / SessionEnd entry points. Default wraps the primer in the Claude Code hook envelope; `text` prints the bare primer; `json` returns `{primer, missed, moreHint}` for the kilo plugin; `codex` prints `{additionalContext}` for Codex CLI hooks and spawns the queue bridge. Exits 3 in `text`/`json` mode when the nick is held by a live peer. |
| `codex-bridge --thread ID [--as NICK] [--foreground]` | Forward incoming messages into a running Codex session via `codex queue` (see "Codex CLI"). Started automatically by `hook-start --emit codex`; detaches unless `--foreground`. |

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
