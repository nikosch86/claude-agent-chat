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

## Subcommands

| Verb | What it does |
| --- | --- |
| `send [--as NICK] <recipient>... 'text'` | Plain message; recipient is one or more `@nick` or `*` for broadcast. Single-quote the body (see Safe sending below). |
| `share [--as NICK] <recipient>... [--file PATH] [--note "..."]` | Copy a file (or stdin) into `~/.agent-chat/artifacts/<sender>/...` and emit a log line referencing the copy. |
| `history [--from @nick] [--to @nick\|me] [--since DUR\|DATE] [--tail N] [--id TS] [--format json\|text]` | Read the log, filter, print. `--id` fetches one message whole by its `ts`, which is what a clipped inbox notice hands you. |
| `peers` | List currently-joined nicks. |
| `listen [--as NICK]` | Stream new lines addressed to you (or broadcast) as raw JSON; designed to be the `Monitor` command. One listener per nick: a newer `listen` takes over and the incumbent exits with a farewell line. |
| `watch [--filter @nick] [--tail N] [--no-color] [--date]` | Live colorized viewer for humans. |
| `chat [--as NICK] [--tail N] [--no-color]` | Interactive read/write client for a human: a scrolling message pane plus a pinned input line with line editing (←/→, Home/End, Delete, ↑/↓ recall history). Prefix a message with `@nick`/`*` to direct or broadcast; no prefix broadcasts. The body is typed, not shell-parsed, so no single-quoting is needed. |
| `reset [<nick>]` | Release a stale nick claim (defaults to the resolver-derived nick). |
| `hook-start [--emit claude\|text\|json]` / `hook-stop` | SessionStart / SessionEnd entry points. Default wraps the primer in the Claude Code hook envelope; `text` prints the bare primer; `json` returns `{primer, missed, moreHint}` for the kilo plugin. Exits 3 in `text`/`json` mode when the nick is held by a live peer. |

Run `agent-chat --help` for the canonical list.

## Opt-outs

- `CLAUDE_AGENT_CHAT=0` — per-session kill switch (read by `hook-start`).
- `<repo>/.no-agent-chat` — per-repo opt-out file at the repo root.

Either one causes `hook-start` to exit cleanly without joining or writing to
the log.

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
   `echo '{}' | AGENT_CHAT_HOME=$TMP agent-chat hook-start` — expect a primer
   JSON on stdout and a `{"event":"joined"}` line in `$TMP/log.jsonl`.
2. **Hook-start in repo B.** Same payload from a second repo. `agent-chat
   peers` should now print both nicks.
3. **Cross-repo send.** From repo A:
   `agent-chat send @<B-nick> 'hi'`. In repo B, run `agent-chat listen` — the
   line should appear within ~1s.
4. **Watch.** In a normal terminal: `agent-chat watch`. Confirm colorized
   output, dim italic join lines, and live follow.
5. **Hook-stop.** `echo '{}' | agent-chat hook-stop` for each session.
   Expect a `quit` line per session and an empty `agents/<nick>/` for each.

Then the crash test: kill a session ungracefully (SIGKILL the hook-stop step
of one of them), start a new one in the same repo, and confirm automatic
stale-state recovery (no manual `reset` needed).
