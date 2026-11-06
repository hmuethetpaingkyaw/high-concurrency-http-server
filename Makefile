# Usage:
#   make up                                  build and start the app (level-0 limits)
#   make load ENDPOINT=/io RATE=100          hold 100 RPS for 1m
#   make ramp ENDPOINT=/io RATE=500          climb 10 -> 500 RPS to find the ceiling
#   make spike ENDPOINT=/io RATE=3000        overload test: fast 503s count as a pass
#   make slowloris CONNS=1000 DURATION=60s   hold slow connections open (run beside a load test)
#   make stats                               live CPU/memory per container
ENDPOINT ?= /io
RATE     ?= 100
DURATION ?= 1m
CONNS    ?= 1000

up:
	docker compose up -d --build

down:
	docker compose down

load:
	docker compose run --rm -e MODE=constant -e ENDPOINT=$(ENDPOINT) -e RATE=$(RATE) -e DURATION=$(DURATION) k6 run /scripts/load.js

ramp:
	docker compose run --rm -e MODE=ramp -e ENDPOINT=$(ENDPOINT) -e RATE=$(RATE) -e DURATION=$(DURATION) k6 run /scripts/load.js

spike:
	docker compose run --rm -e MODE=spike -e ENDPOINT=$(ENDPOINT) -e RATE=$(RATE) -e DURATION=$(DURATION) k6 run /scripts/load.js

slowloris:
	docker compose run --rm slowloris -conns $(CONNS) -duration $(DURATION)

stats:
	docker stats

logs:
	docker compose logs -f app

.PHONY: up down load ramp spike slowloris stats logs
