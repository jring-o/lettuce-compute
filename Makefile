# DEVELOPMENT-stack shortcuts ONLY.
#
# Every target below pins the dev compose file (-f compose.yaml) AND a dedicated
# compose project (-p lettuce-dev). Both are required: `-f compose.yaml` alone
# does NOT isolate the stack, because compose still defaults the PROJECT NAME to
# the working-directory name — the same default a production stack started from
# this directory uses. Sharing a project name means a bare `make down` here would
# tear down the production containers. Pinning a distinct project name makes these
# targets structurally unable to touch a production stack.
#
# Production is deployed ONLY via guides/head-setup.md, which uses
# -f compose.production.yaml. This Makefile deliberately has NO production
# targets: adding them would recreate the very accident this split prevents, just
# in reverse (a stray `make` target reaching into the production project).
#
# ONE-TIME MIGRATION: switching to `-p lettuce-dev` orphans the containers and
# volumes created under the OLD default project name (the directory name). Before
# your first `make dev-up`, stop them once under that old project name with a bare
# `docker compose down` (no -p) so they do not linger. This is a comment; the CI
# guardrail that forbids un-pinned `docker compose` invocations ignores comments.

.PHONY: dev-up dev-down dev-logs dev-rebuild dev-reset

COMPOSE_DEV := docker compose -f compose.yaml -p lettuce-dev

dev-up:
	$(COMPOSE_DEV) up -d

dev-down:
	$(COMPOSE_DEV) down

dev-logs:
	$(COMPOSE_DEV) logs -f

dev-rebuild:
	$(COMPOSE_DEV) build --no-cache

dev-reset:
	@printf "This DELETES the dev database volume (project lettuce-dev). Type 'dev' to continue: " && read ans && [ "$$ans" = "dev" ] && $(COMPOSE_DEV) down -v || { echo "aborted — nothing was removed"; exit 1; }
