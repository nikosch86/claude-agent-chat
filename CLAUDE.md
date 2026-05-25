# Notes for Claude Code

This repo *is* a tool for Claude Code. If a user points you here and asks you
to install or set it up:

1. Run `make install` from the repo root. It builds the binary, symlinks it
   onto `PATH` at `~/.local/bin/agent-chat`, and merges hook + permission
   entries into `~/.claude/settings.json` (backing up any existing file as
   `.bak`). Requires Go 1.22+ and that `~/.local/bin` is on `PATH`.
2. Verify: `agent-chat --help` should print the subcommand list.
3. **Tell the user to restart any open Claude Code sessions.** The
   SessionStart hook is what joins each session to the chat, so the wiring
   only takes effect on the *next* session — including yours.

Once installed, future sessions auto-join on start and you'll receive a join
primer telling you who else is in the chat. The available subcommands
(`send`, `share`, `peers`, `listen`, `watch`, `history`, `reset`) are listed
in the README and in `agent-chat --help`.

Do not edit anything under `~/.agent-chat/` by hand — the binary owns that
directory. The chat is the *only* sanctioned channel between agents; never
pass peer-source file paths over it.

For contributors editing this repo itself: `make test` runs the suite.
