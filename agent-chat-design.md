# Agent Chat — local multi-agent communication

A single shared append-only JSONL log on the local filesystem. **One CLI binary** is the only interface; humans use the same binary as agents. No daemon, no server.

This document is the scoped spec. Language is intentionally not specified — that decision comes after this is agreed.

---

## Goals

1. Two or more agents can talk to each other.
2. Agents wake up (react) when addressed.
3. The human can watch all of it.
4. Files and context can be shared efficiently.

## Non-goals (explicitly out of v1)

- Channels / rooms — one shared room only.
- DM privacy — the log is plaintext to anyone with filesystem access.
- Cross-machine chat — local-only by design.
- Authentication / ACLs.
- Log rotation — manual for now; a `compact` subcommand can be added later.
- Heartbeats / liveness daemons — log-activity-based staleness is sufficient.
- Threading / reply correlation in the wire schema.
- PreToolUse hook injecting unread-count into every tool call.

---

## Alternatives considered and rejected

Preserved from the original brief so future sessions don't relitigate:

1. **Extending in-house TUI chat (Go, TCP)** — half a day of work, off-the-shelf was faster.
2. **ngIRCd + ii** — 512-byte per-line wire limit truncates real messages; per-agent daemon adds ops.
3. **hcom** — too coupled to Claude Code's launcher; not generic.
4. **NATS / Redis pub-sub** — not chat-shaped, no human-watchable transcript.
5. **Matrix / Mattermost / etc** — overkill, docker-compose, REST clients.
6. **Sub-agent spawning** — loses peer state when they're already running.

---

## Architecture overview

- One append-only file is the truth: `~/.agent-chat/log.jsonl`.
- One executable is the whole installation: `~/.claude/agent-chat/agent-chat`. It is the wire-format implementer, the CLI for both humans and agents, and the hook implementer.
- Hooks call subcommands of the same binary (`hook-start`, `hook-stop`) — there are no shim scripts.
- No background daemons. The only long-running process per agent is the `Monitor(agent-chat listen)` the agent starts itself; that process's lifetime is the Monitor's lifetime.

### File layout

```
~/.agent-chat/
├── log.jsonl                            # append-only, the single source of truth
├── by-cwd/
│   └── <sha256(cwd-or-git-root)>.nick   # cwd → nick map; runtime lookup target
├── agents/
│   └── <nick>/
│       └── cursor                       # last-read byte offset into log.jsonl
└── artifacts/
    └── <sender-nick>/
        └── <unix-ms>-<hash>-<basename>  # copied content for sharing

~/.config/agent-chat/nick                # optional humans' default nick override
```

There is no inbox file, no listener PID file, no per-session marker beyond the by-cwd mapping. `log.jsonl` plus tiny per-agent state is everything.

---

## Wire schema

Newline-delimited JSON. Required: `ts`, `from`. Optional: `to`, `text`, `path`, `event`, `note`. **No message IDs. No `reply_to`. No threading.**

```json
{"ts":1779619000.500,"from":"infra-ansible","to":"@foo-service","text":"env vars at startup?"}
{"ts":1779619001.200,"from":"foo-service","to":"@infra-ansible","text":"FOO_PORT=8080. main.go:42."}
{"ts":1779619002.000,"from":"foo-service","event":"joined"}
{"ts":1779619100.000,"from":"foo-service","event":"quit"}
{"ts":1779619300.000,"from":"foo-service","to":"@infra-ansible","path":"/home/u/.agent-chat/artifacts/foo-service/1779619300000-a3f7b2-unit.service","note":"draft unit"}
{"ts":1779619400.000,"from":"infra-ansible","to":"*","text":"deploy in 5min"}
```

Field meanings:

- `ts` — epoch seconds, millisecond precision.
- `from` — sender nick.
- `to` — `@<nick>` for directed, `*` for broadcast, omitted for system events.
  **Always a single string.** Multi-recipient is CLI sugar that emits N lines (see `send` below).
- `text` — inline payload. No hard cap, but prefer `share` for anything more than a short answer.
- `path` — absolute filesystem path. **Always under `~/.agent-chat/artifacts/`.** The wire never carries a peer's source-repo path.
- `event` — `joined` or `quit`. Emitted at hook-start / hook-stop and at `watch` start/exit.
- `note` — short human context paired with `path`.

---

## Sovereignty rule (the key invariant)

> **Each agent is authoritative for its own repo. The chat is the only interface between agents and is the only thing that crosses repo boundaries. Every file or content chunk that goes through the chat is copied into `~/.agent-chat/artifacts/<sender-nick>/` first; the wire never carries a path outside `artifacts/`.**

This is enforced structurally by `send` and `share` (the binary writes the artifact and emits the artifact path, never the source path) and reinforced by a primer rule the agent reads on join. Read permissions are not sandboxed — an agent that decides to read elsewhere can — but the chat never gives them a reason or a reference to.

---

## Identity

Nicks are derived once per session at hook-start. The `by-cwd/<hash>.nick` mapping is the runtime source of truth for "who am I."

### At hook-start

Decision tree (first match wins):

1. `CLAUDE_AGENT_CHAT=0` → skip (chat disabled for this session).
2. `<cwd>/.no-agent-chat` exists → skip (chat disabled for this repo).
3. Derive nick:
   - `CLAUDE_AGENT_CHAT_NICK` env var, or
   - git repo basename (via `git rev-parse --show-toplevel`), or
   - `<cwd>/.agent-chat-nick` first non-empty line.
   - None of the above → skip.
4. Sanitize: `[A-Za-z0-9_-]`, max 24 chars, no leading/trailing dashes.
5. **Collision check** (see below) — refuse with explicit primer if conflict.
6. On success: write `by-cwd/<sha256-of-git-root-or-cwd>.nick`, write `agents/<nick>/cursor` (initialised to current log size or preserved from last session), append `{event: joined}` to log, emit the join primer.

### Collision (two sessions in the same repo)

The second session's hook-start sees that the nick is already claimed by an active session. It refuses with an explicit `additionalContext`:

```
## Agent Chat: NOT JOINED

Nick `foo-service` is already in use by another active session. To join with
a distinct identity, restart this session with
CLAUDE_AGENT_CHAT_NICK=foo-service-2 (or any other unused nick). To proceed
without chat, ignore this notice.
```

No auto-suffix. The "one nick = one repo's authority" invariant is preserved.

### Stale-state recovery (hook-stop didn't run)

If a previous session crashed (SIGKILL, machine reboot, terminal closed without graceful exit), its `by-cwd` mapping is still there but it's dead.

- **Automatic:** the collision check in step 5 looks at `log.jsonl` for any activity from the claimed nick in the last 30 minutes. If none, treat as stale → append a synthetic `{event: quit}` for that nick, clear `by-cwd` and `agents/<nick>` entries, claim the nick.
- **Manual:** `agent-chat reset [<nick>]` does the same explicitly. For when you've just crashed and want to start over immediately without waiting for the staleness window.

### At runtime — unified nick resolver

Used by every CLI invocation other than `hook-start` to answer "who is calling me right now?":

1. `--as NICK` flag.
2. `$AGENT_CHAT_NICK` env var (rarely set in agent context; useful for humans).
3. `by-cwd/<sha256-of-cwd-or-git-root>.nick` lookup (agents — the file the hook wrote).
4. `~/.config/agent-chat/nick` (humans, set once).
5. `$USER` (humans, ultimate fallback).

If none yields a registered nick (e.g. an agent in a session that was collision-refused), the binary refuses with a message that names the cause and points at the env-var override.

---

## CLI verbs

### `agent-chat send <recipient>... "text"`

Plain text. `<recipient>` is one or more `@nick`, or `*` for broadcast.

Multi-recipient is CLI sugar: `agent-chat send @alice @bob "..."` emits two log lines (one `@alice`, one `@bob`). The wire stays single-recipient.

### `agent-chat share <recipient>... [--file PATH] [--note "..."]`

Copy content into `artifacts/<sender>/<unix-ms>-<6char-hash>-<basename>` and emit a `{path, note}` message. Sources:

- `--file PATH` → read from PATH.
- Stdin pipe → read stdin (used for ephemeral content).
- Both modes mutually exclusive at the parser.

### `agent-chat listen`

Streams new lines from `log.jsonl` (starting at the agent's cursor, forward), filters to `to == "@me" || to == "*"` in-process, prints **raw JSON** one line per match. Designed to be the Monitor command. Advances the cursor on every emit.

### `agent-chat watch [--filter @nick] [--tail N] [--no-color] [--date]`

Live colorized viewer (humans' primary surface). Emits `{event: joined}` on start, `{event: quit}` on exit (SIGINT / EOF / clean shutdown). Defaults:

- Show last 30 lines, then follow forever.
- Per-nick stable ANSI color (`hash(nick) → color`).
- Dim italic for join/quit lines.
- `HH:MM:SS` timestamps (`--date` switches to `YYYY-MM-DD HH:MM:SS` for review).
- Path lines show `[file] <path> — <note>`; no auto-peek into the artifact.
- All traffic, not just direct messages. `--filter @nick` narrows.

### `agent-chat peers`

Lists currently-joined nicks (joined events with no subsequent quit). Cheap; scans the log.

### `agent-chat history [--to @nick|me] [--from @nick] [--since DUR|DATE] [--tail N] [--format json|text]`

Reads the log, filters, prints. Default `--format json` (machine-readable). `text` uses the same renderer as `watch`. Used by agents to catch up after a reconnect (`history --to me --since 1h`) and by humans for review.

### `agent-chat reset [<nick>]`

Force-cleanup stale state for a nick. Emits a `quit` event, removes the `by-cwd` mapping and `agents/<nick>/cursor`. No-op if the nick isn't claimed.

### `agent-chat hook-start`

Reads SessionStart hook JSON from stdin. Executes the hook-start decision tree above. Writes the `additionalContext` JSON to stdout (the join primer).

### `agent-chat hook-stop`

Reads SessionEnd hook JSON from stdin. Appends `{event: quit}` to the log, advances the cursor to current EOF (the "last-seen offset" semantics), removes the by-cwd mapping for this session.

---

## Reactive listening

Agents are reactive via a single Monitor invocation at session start, told to them in the primer:

```
Monitor(command="agent-chat listen", persistent: true, description: "agent-chat inbox")
```

Each matching line printed by `listen` becomes a Monitor notification (raw JSON). The cursor advances on each emit, so a session crash loses at most the messages that arrived between the last emit and the crash — typically zero.

### Listener self-heal

Monitor can stop unexpectedly (high-volume auto-stop, script error). The agent has no inbuilt signal that this happened. To avoid silent dropout:

Every `agent-chat` subcommand other than `listen` itself checks: has the cursor advanced recently *and* is there matching traffic past it in the log? If the listener appears stopped, the binary prints to **stderr**:

```
[agent-chat] listener appears stopped — you have N unread; run Monitor(agent-chat listen, persistent: true)
```

Costs nothing when fine. Self-heals on next CLI invocation when broken.

---

## Replay on rejoin

At hook-start (after a clean rejoin or a crash recovery), the binary reads `log.jsonl` from the saved cursor to EOF, filters for "addressed to me or broadcast," and includes the missed lines **inline in the join primer text** (as `additionalContext`). The cursor is then advanced to EOF; `agent-chat listen` (when the agent starts it) streams only what arrives from then on.

Missed-while-offline messages are catch-up history shown once; new-while-online messages are notifications. Two paths, each carrying the form that fits.

---

## Artifacts lifecycle

- Sender's binary writes content to `artifacts/<sender-nick>/<unix-ms>-<6char-hash>-<basename>`.
  - `<unix-ms>` — sort and mtime-prune robustness.
  - `<6char-hash>` — collision guard on rapid double-shares.
  - `<basename>` — human legibility when browsing.
- The wire references that path. Receivers `Read` it.
- **Cleanup:** at `hook-start`, prune any artifact whose mtime is older than 14 days.
- Per-sender subdir means cleanup is auditable and one runaway agent doesn't pollute everyone else's space.

---

## The join primer (the text agents see on connect)

Short, three rules. The fields are filled in by `hook-start`:

```
## Agent Chat is active

You are joined as `foo-service`. Active peers: alice, bob, hoffmann.

You missed 3 mentions while offline:
  {"ts":...,"from":"alice","to":"@foo-service","text":"..."}
  {"ts":...,"from":"bob","to":"*","text":"..."}
  {"ts":...,"from":"alice","to":"@foo-service","path":"~/.agent-chat/artifacts/alice/...","note":"..."}

To stay reactive, run this once now:
  Monitor(command="agent-chat listen", persistent: true, description: "agent-chat inbox")

Commands:
  agent-chat send @peer "..."             # plain reply
  agent-chat share @peer --file PATH      # share a file (auto-copied to artifacts)
  agent-chat peers                        # who's around
  agent-chat history --to me              # what's been said to me
  agent-chat --help                       # everything else

Rules:
  - You are the authority on this repo (`foo-service`). Peers ask you about it.
  - Do NOT read peer repos directly. If a peer's content matters, ask them or wait for them to `share` it. Any `path` you receive will live under ~/.agent-chat/artifacts/.
  - Questions are async: send and continue working. When a reply lands as a listen notification, respond then. If a peer doesn't answer for a long time, escalate by addressing @hoffmann.
```

For collision-refused sessions, the "NOT JOINED" primer from the Identity section is emitted instead.

---

## Opt-outs

- `CLAUDE_AGENT_CHAT=0` env var — per-session kill switch.
- `<repo>/.no-agent-chat` file at repo root — per-repo opt-out.

---

## Settings (settings.json wiring)

Hooks (per Claude Code's hooks format):

- SessionStart → `~/.claude/agent-chat/agent-chat hook-start`
- SessionEnd  → `~/.claude/agent-chat/agent-chat hook-stop`

Permissions (rough shape — path-scoped, agent-only):

- `Bash(agent-chat send *)`
- `Bash(agent-chat share *)`
- `Bash(agent-chat peers)`
- `Bash(agent-chat history *)`
- `Bash(agent-chat reset *)`
- `Monitor(agent-chat listen)`
- `Read(/home/<user>/.agent-chat/artifacts/**)` — the only Read scope the chat needs.

Concrete patterns get filled in once the install path and binary name are final.

---

## Known limits and failure modes

- **`log.jsonl` grows unbounded.** Rotate manually for now. `agent-chat compact` later if it becomes a problem.
- **Stale-window false positives.** A genuinely silent agent (no chat traffic for 30+ minutes) can be reclaimed by a same-repo session as "stale." Tune the window if it bites.
- **Sovereignty is structural, not enforced.** The wire never carries peer-source paths, and the primer rule reinforces it, but `Read` is not sandboxed. An agent that decides to read peer source paths it learned outside the chat will succeed.
- **Multiple watches per human.** Each `watch` emits join/quit. The peer list dedups by nick; the staleness check works on log activity, not watch presence — so a quiet watch isn't proof of life on its own.
- **Cursor file corruption.** If the cursor points past current EOF (log rotated/truncated externally), the binary resets it to 0 on next `listen` / `history` invocation and replays from start. Documented in the same place as log rotation.

---

## Build checklist (for a fresh session implementing this)

1. Write the single binary at `~/.claude/agent-chat/agent-chat`. Subcommands: `send`, `share`, `listen`, `watch`, `peers`, `history`, `reset`, `hook-start`, `hook-stop`. Stdlib-only is the goal once a language is chosen.
2. Wire the two hooks in `~/.claude/settings.json` to the corresponding subcommands.
3. Add the permission patterns (filled in for the concrete install path).
4. Smoke test:
   - Pipe a fake SessionStart payload into `agent-chat hook-start` from a git repo; confirm the join primer JSON and `joined` log line.
   - From a second repo, repeat; both nicks should appear in `agent-chat peers`.
   - From repo A: `agent-chat send @<B-nick> "hi"`. From repo B's session: it lands as a listen notification.
   - In a normal terminal: `agent-chat watch`. Confirm color, dim joins, follow.
   - Pipe a SessionEnd payload to `agent-chat hook-stop`; confirm `quit` line, cleared state.
5. Crash test: kill a session ungracefully, start a new one in the same repo, confirm automatic stale-state recovery.
