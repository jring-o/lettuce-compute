.PHONY: up down logs rebuild reset

up:
	docker compose up -d

down:
	docker compose down

logs:
	docker compose logs -f

rebuild:
	docker compose build --no-cache

reset:
	docker compose down -v
