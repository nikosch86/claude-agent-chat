// agent-chat plugin for the kilo CLI.
//
// Bridges the agent-chat binary into kilo the way the SessionStart hook +
// `Monitor(agent-chat listen)` do for Claude Code:
//
//   session.created  -> claim nick, write a join record, expose the primer
//   <listener>       -> stream messages addressed to this nick into the session
//   session.idle     -> inject any queued messages (never mid-turn)
//   session.deleted  -> write a quit record and stop the listener
//
// The join primer is surfaced as SYSTEM context (chat.system.transform), not a
// user turn — a user-role primer makes eager models act on it unprompted.
// Incoming peer messages ARE injected as user turns, so the agent reacts; the
// capped missed mentions returned by `hook-start --emit json` are injected once
// as a catch-up turn (the kilo equivalent of Claude's session-start backlog).
//
// The binary owns all chat state; this file only shells out to it. Register it
// via the kilo installer (adds an absolute path to the `plugin` array in
// kilo.jsonc) or `kilo plugin`. Set AGENT_CHAT_PLUGIN_DEBUG=1 to trace to
// ~/.config/kilo/agent-chat-debug.log.

import { spawn } from "node:child_process";
import { existsSync, appendFileSync } from "node:fs";
import { homedir } from "node:os";
import { join } from "node:path";

// Quiet by default; set AGENT_CHAT_PLUGIN_DEBUG to trace the bridge.
const DEBUG = !!process.env.AGENT_CHAT_PLUGIN_DEBUG;
const DEBUG_LOG = join(homedir(), ".config", "kilo", "agent-chat-debug.log");
function dbg(...parts) {
  if (!DEBUG) return;
  try {
    appendFileSync(DEBUG_LOG, `${new Date().toISOString()} ${parts.join(" ")}\n`);
  } catch {
    /* best effort */
  }
}

// Exit code hook-start --emit text returns when the nick is already held by a
// live peer, meaning we did NOT join and must not start a listener under it.
const RC_NOT_JOINED = 3;

// stdin is "ignore" (/dev/null) so the child sees immediate EOF — the binary
// drains stdin before doing work and would otherwise block forever on the open
// pipe Node hands a spawned child.
const STDIO = ["ignore", "pipe", "pipe"];

// Resolve the agent-chat binary: explicit override, then the paths the
// installer writes, then bare name (PATH). Survives kilo's process PATH
// omitting ~/.local/bin.
function resolveBin() {
  if (process.env.AGENT_CHAT_BIN) return process.env.AGENT_CHAT_BIN;
  for (const c of [
    join(homedir(), ".local", "bin", "agent-chat"),
    join(homedir(), ".claude", "agent-chat", "agent-chat"),
  ]) {
    if (existsSync(c)) return c;
  }
  return "agent-chat";
}

// Run the binary to completion; resolve with its stdout and exit code.
function capture(bin, args, cwd) {
  return new Promise((resolve) => {
    let out = "";
    const child = spawn(bin, args, { cwd, env: process.env, stdio: STDIO });
    child.stdout.on("data", (c) => (out += c.toString()));
    child.on("error", (e) => {
      dbg("capture error", JSON.stringify(args), String(e));
      resolve({ out: "", code: -1 });
    });
    child.on("close", (code) => {
      dbg("capture done", JSON.stringify(args), "code=" + code, "len=" + out.length);
      resolve({ out, code });
    });
  });
}

// Render one raw listen line (JSON) into a compact string; raw on parse miss.
function formatLine(line) {
  let r;
  try {
    r = JSON.parse(line);
  } catch {
    return line;
  }
  const who = r.from ? "@" + r.from : "someone";
  if (r.path) return `${who} shared a file: ${r.path}${r.note ? ` — ${r.note}` : ""}`;
  if (typeof r.text === "string") return `${who}: ${r.text}`;
  return line;
}

export const AgentChatPlugin = async ({ client }) => {
  const bin = resolveBin();
  const sessions = new Map(); // sessionID -> { dir, idle, flushing, queue, primer, listener }

  // Inject text as a user turn (wakes the agent to react). Returns success.
  async function prompt(id, dir, text) {
    try {
      await client.session.prompt({
        path: { id },
        query: { directory: dir },
        body: { parts: [{ type: "text", text }] },
      });
      dbg("inject ok", "id=" + id, "len=" + text.length);
      return true;
    } catch (e) {
      dbg("inject failed", "id=" + id, String(e?.message || e));
      return false;
    }
  }

  // Deliver pending traffic, one turn per idle so we never clobber an in-flight
  // turn (injecting starts a turn → back to busy until the next session.idle).
  // Missed mentions go first, once, as a distinct catch-up turn — the kilo
  // equivalent of how Claude surfaces missed mentions at session start.
  async function flush(s, id) {
    if (!s.idle || s.flushing) return;

    if (!s.caughtUp && s.missed.length) {
      s.flushing = true;
      s.idle = false;
      const body = s.missed.map(formatLine).join("\n");
      const more = s.moreHint ? `\n(latest ${s.missed.length} shown — run \`${s.moreHint}\` for the rest)` : "";
      if (await prompt(id, s.dir, `While you were away you received message(s):\n${body}${more}`)) s.caughtUp = true;
      s.flushing = false;
      return;
    }

    if (s.queue.length === 0) return;
    s.flushing = true;
    s.idle = false;
    const batch = s.queue.splice(0);
    const header = batch.length === 1 ? "New agent-chat message" : `${batch.length} new agent-chat messages`;
    if (!(await prompt(id, s.dir, `${header}:\n${batch.map(formatLine).join("\n")}`))) {
      s.queue.unshift(...batch); // re-queue for the next idle
    }
    s.flushing = false;
  }

  function startListener(id, s) {
    const child = spawn(bin, ["listen"], { cwd: s.dir, env: process.env, stdio: STDIO });
    s.listener = child;
    let buf = "";
    child.stdout.on("data", (chunk) => {
      buf += chunk.toString();
      let nl;
      while ((nl = buf.indexOf("\n")) >= 0) {
        const line = buf.slice(0, nl).trim();
        buf = buf.slice(nl + 1);
        if (!line || line.startsWith("[agent-chat]")) continue; // skip farewell/self-heal notices
        s.queue.push(line);
        flush(s, id).catch(() => {});
      }
    });
    child.on("error", () => {});
  }

  // Stop the listener and write the quit record for one session. Idempotent:
  // hook-stop is a no-op once the by-cwd entry it removes is gone.
  async function leave(id) {
    const s = sessions.get(id);
    if (!s) return;
    try {
      s.listener?.kill();
    } catch {
      /* already gone */
    }
    sessions.delete(id);
    await capture(bin, ["hook-stop"], s.dir);
  }

  return {
    // Expose the join primer as system context for joined sessions. Fires per
    // request; output.system is rebuilt each time, so append once per call.
    "experimental.chat.system.transform": async (input, output) => {
      const s = input?.sessionID && sessions.get(input.sessionID);
      if (s?.primer) output.system.push(s.primer);
    },

    event: async ({ event }) => {
      const t = event?.type;

      if (t === "session.created") {
        const info = event.properties.info;
        if (info.parentID) return; // only primary sessions join the chat
        const dir = info.directory;
        if (!dir) return; // no working dir → can't derive a nick; skip
        // hook-start claims the nick (git root of `dir`), writes the join
        // record, computes missed mentions, and returns {primer, missed,
        // moreHint}. The listener resolves the same nick from `dir` via the
        // by-cwd entry it just wrote and starts at EOF (no missed-replay).
        const { out, code } = await capture(bin, ["hook-start", "--emit", "json"], dir);
        if (code === RC_NOT_JOINED) return; // nick held by a live peer — don't hijack its inbox
        let data;
        try {
          data = JSON.parse(out);
        } catch {
          return; // opted out (CLAUDE_AGENT_CHAT=0 / .no-agent-chat) → no output
        }
        if (!data?.primer) return;
        dbg("created", "id=" + info.id, "dir=" + dir, "missed=" + (data.missed?.length || 0));
        // idle starts false: the session may already be mid-(first-)turn. Only a
        // confirmed session.idle flips it true, so we never inject mid-turn.
        const s = {
          dir,
          idle: false,
          flushing: false,
          queue: [],
          primer: data.primer,
          missed: Array.isArray(data.missed) ? data.missed : [],
          moreHint: data.moreHint || "",
          caughtUp: false,
        };
        sessions.set(info.id, s);
        startListener(info.id, s);
        return;
      }

      if (t === "session.idle") {
        const s = sessions.get(event.properties.sessionID);
        if (s) {
          s.idle = true;
          await flush(s, event.properties.sessionID);
        }
        return;
      }

      if (t === "session.deleted") {
        await leave(event.properties.info.id);
        return;
      }
    },

    // Server shutting down (e.g. the TUI was closed). kilo persists sessions
    // and fires no session.deleted here, so write the quit records now —
    // otherwise the nick lingers as a ghost peer with a dead listener until the
    // staleness window reclaims it.
    dispose: async () => {
      await Promise.all([...sessions.keys()].map(leave));
    },
  };
};

export default AgentChatPlugin;
