# Local development only. See AGENTS.md and docker-compose.dev.yml.
-include .env
export

.PHONY: dev dev-db dev-db-down dev-db-logs dev-backend dev-frontend

dev-db:
	docker compose -f docker-compose.dev.yml up -d

dev-db-down:
	docker compose -f docker-compose.dev.yml down

dev-db-logs:
	docker compose -f docker-compose.dev.yml logs -f

dev-backend:
	go run ./backend/bin/server/main.go --port 8080 --data . --debug

dev-frontend:
	pnpm --dir frontend dev

# Starts the dev database, backend, and frontend together; Ctrl+C stops all three.
dev: dev-db
	@trap 'kill 0' EXIT INT TERM; \
	$(MAKE) dev-backend & \
	$(MAKE) dev-frontend & \
	wait
