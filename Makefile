PREFIX     ?= $(HOME)/.claude/agent-chat
PATHDIR    ?= $(HOME)/.local/bin
BIN        := agent-chat
INSTALLED  := $(PREFIX)/$(BIN)
SYMLINK    := $(PATHDIR)/$(BIN)

.PHONY: build install uninstall install-kilo uninstall-kilo install-codex uninstall-codex test smoke clean

build:
	go build -o $(BIN) .

install: build
	@mkdir -p $(PREFIX) $(PATHDIR)
	install -m 0755 $(BIN) $(INSTALLED)
	@echo "installed binary -> $(INSTALLED)"
	@ln -sf $(INSTALLED) $(SYMLINK)
	@echo "linked $(SYMLINK) -> $(INSTALLED)"
	go run ./installer

uninstall:
	go run ./installer --uninstall
	@rm -f $(SYMLINK)
	@echo "removed $(SYMLINK)"
	@rm -f $(INSTALLED)
	@echo "removed $(INSTALLED)"
	@if [ -d $(PREFIX) ] && [ -z "$$(ls -A $(PREFIX))" ]; then rmdir $(PREFIX); fi

# kilo CLI wiring: installs the binary + PATH symlink like `install`, then
# registers the kilo plugin and permission entry instead of the Claude hooks.
install-kilo: build
	@mkdir -p $(PREFIX) $(PATHDIR)
	install -m 0755 $(BIN) $(INSTALLED)
	@echo "installed binary -> $(INSTALLED)"
	@ln -sf $(INSTALLED) $(SYMLINK)
	@echo "linked $(SYMLINK) -> $(INSTALLED)"
	go run ./kilo

# Removes only the kilo wiring; leaves the shared binary + symlink in place
# (a Claude install may still depend on them). Run `make uninstall` to remove those.
uninstall-kilo:
	go run ./kilo --uninstall

# Codex CLI wiring: installs the binary + PATH symlink like `install`, then
# merges hook entries into ~/.codex/hooks.json and the sandbox writable-root
# into ~/.codex/config.toml instead of the Claude hooks.
install-codex: build
	@mkdir -p $(PREFIX) $(PATHDIR)
	install -m 0755 $(BIN) $(INSTALLED)
	@echo "installed binary -> $(INSTALLED)"
	@ln -sf $(INSTALLED) $(SYMLINK)
	@echo "linked $(SYMLINK) -> $(INSTALLED)"
	go run ./codex

# Removes only the Codex wiring; leaves the shared binary + symlink in place
# (a Claude install may still depend on them). Run `make uninstall` to remove those.
uninstall-codex:
	go run ./codex --uninstall

test:
	go test ./...

# Run the 5-step build-checklist smoke test from agent-chat-design.md.
# See README.md "Smoke test" for what each step verifies.
smoke:
	@echo "Run the smoke test manually — see README.md \"Smoke test\" for the 5 steps."
	@echo "Use AGENT_CHAT_HOME=\$$(mktemp -d) to keep the test out of your real ~/.agent-chat."

clean:
	rm -f $(BIN)
