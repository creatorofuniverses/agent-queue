GO        ?= go
BINDIR    ?= $(HOME)/.local/bin
UNITDIR   ?= $(HOME)/.config/systemd/user
CONFDIR   ?= $(HOME)/.config/agent-queue
STATEDIR  ?= $(HOME)/.local/state/agent-queue
SYSTEMCTL ?= systemctl --user
LOGINCTL  ?= loginctl
SKILLDIRS ?= $(HOME)/.claude/skills $(HOME)/.codex/skills

.PHONY: build test test-integration test-install install uninstall purge

build:
	$(GO) build -o bin/aq ./cmd/aq

test:
	$(GO) test -race ./...

test-install:
	bash test/install_test.sh

test-integration:
	$(GO) test -tags integration -p 1 ./...

install: build
	install -d $(BINDIR) $(UNITDIR) $(CONFDIR)
# Replace by rename: the old binary may still be running.
	install -m 0755 bin/aq $(BINDIR)/.aq.new
	mv -f $(BINDIR)/.aq.new $(BINDIR)/aq
	install -m 0644 deploy/systemd/aq.slice $(UNITDIR)/aq.slice
ifeq ($(BINDIR),$(HOME)/.local/bin)
	install -m 0644 deploy/systemd/aq.service $(UNITDIR)/aq.service
else
	sed 's|%h/.local/bin/aq|$(BINDIR)/aq|' deploy/systemd/aq.service > $(UNITDIR)/aq.service
	chmod 0644 $(UNITDIR)/aq.service
endif
	test -e $(CONFDIR)/config.toml || install -m 0600 deploy/config.example.toml $(CONFDIR)/config.toml
	@set -eu; for dir in $(SKILLDIRS); do \
		install -d "$$dir"; stage=$$(mktemp -d "$$dir/.aq.XXXXXX"); \
		trap 'if test -d "$$stage/old" && ! test -e "$$dir/aq"; then mv "$$stage/old" "$$dir/aq"; fi; rm -rf "$$stage"' EXIT; \
		cp -R deploy/skills/aq "$$stage/aq"; \
		if test -e "$$dir/aq"; then mv "$$dir/aq" "$$stage/old"; fi; \
		mv "$$stage/aq" "$$dir/aq"; rm -rf "$$stage"; trap - EXIT; \
	done
	@echo "hooks are opt-in: see deploy/hooks/claude-settings.json and deploy/hooks/codex-hooks.json"
	$(SYSTEMCTL) daemon-reload
	$(SYSTEMCTL) enable aq.service
# restart rather than start: a reinstall must pick up the new binary.
	$(SYSTEMCTL) restart aq.service
	@if [ "$$($(LOGINCTL) show-user "$$(id -un)" -p Linger --value 2>/dev/null)" != yes ]; then \
		echo "hint: linger is off; aq and its tasks stop when you log out."; \
		echo "      enable it with: loginctl enable-linger $$(id -un)"; \
	fi

# Keeps the database, logs and config.
uninstall:
	-$(SYSTEMCTL) disable --now aq.service
	rm -f $(BINDIR)/aq $(UNITDIR)/aq.service $(UNITDIR)/aq.slice
	@for dir in $(SKILLDIRS); do test -n "$$dir" && rm -rf "$$dir/aq"; done
	$(SYSTEMCTL) daemon-reload

# Also deletes the database and all task logs. The config is kept.
purge: uninstall
	rm -rf $(STATEDIR)
