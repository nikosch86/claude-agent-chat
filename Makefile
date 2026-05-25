PREFIX     ?= $(HOME)/.claude/agent-chat
PATHDIR    ?= $(HOME)/.local/bin
BIN        := agent-chat
INSTALLED  := $(PREFIX)/$(BIN)
SYMLINK    := $(PATHDIR)/$(BIN)

.PHONY: build install uninstall test smoke clean

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

test:
	go test ./...

# Run the 5-step build-checklist smoke test from agent-chat-design.md.
# See README.md "Smoke test" for what each step verifies.
smoke:
	@echo "Run the smoke test manually — see README.md \"Smoke test\" for the 5 steps."
	@echo "Use AGENT_CHAT_HOME=\$$(mktemp -d) to keep the test out of your real ~/.agent-chat."

clean:
	rm -f $(BIN)
