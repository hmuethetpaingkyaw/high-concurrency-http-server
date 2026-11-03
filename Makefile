# Usage:
#   make up                                  build and start the app (level-0 limits)
#   make load ENDPOINT=/io RATE=100          hold 100 RPS for 1m
#   make ramp ENDPOINT=/io RATE=500          climb 10 -> 500 RPS to find the ceiling
#   make stats                               live CPU/memory per container
ENDPOINT ?= /io
RATE     ?= 100
DURATION ?= 1m

up:
	docker compose up -d --build

down:
	docker compose down

load:
	docker compose run --rm -e MODE=constant -e ENDPOINT=$(ENDPOINT) -e RATE=$(RATE) -e DURATION=$(DURATION) k6 run /scripts/load.js

ramp:
	docker compose run --rm -e MODE=ramp -e ENDPOINT=$(ENDPOINT) -e RATE=$(RATE) -e DURATION=$(DURATION) k6 run /scripts/load.js

stats:
	docker stats

logs:
	docker compose logs -f app

.PHONY: up down load ramp stats logs
