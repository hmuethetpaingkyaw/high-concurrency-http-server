# Usage:
#   make up                                  build and start the app (level-0 limits)
#   make load ENDPOINT=/io RATE=100          hold 100 RPS for 1m
#   make ramp ENDPOINT=/io RATE=500          climb 10 -> 500 RPS to find the ceiling
#   make spike ENDPOINT=/io RATE=3000        overload test: fast 503s count as a pass
#   make slowloris CONNS=1000 DURATION=60s   hold slow connections open (run beside a load test)
#   make stats                               live CPU/memory per container
# Level 3 (see docs/LEVEL3_GUIDE.md):
#   make load ENDPOINT='/products/{id}' RATE=500       {id} = product id (80% hot)
#   make load ENDPOINT='/products?category={cat}' RATE=200
#   make load ENDPOINT=/orders METHOD=POST RATE=200
#   make vars                                app counters: DB queries/connections/errors, cache, pool
#   make pgconns                             connections Postgres sees, by state
#   make db-freeze FREEZE=10                 freeze Postgres for 10s (a stuck database)
#   make psql / make db-reset
ENDPOINT ?= /io
RATE     ?= 100
DURATION ?= 1m
CONNS    ?= 1000
METHOD   ?= GET
HOT_KEYS ?= 1000
HOT_RATIO ?= 0.8
FREEZE   ?= 10

# Quoted: endpoints like /products?category={cat} contain shell glob characters.
K6_ENV = -e ENDPOINT='$(ENDPOINT)' -e RATE=$(RATE) -e DURATION=$(DURATION) \
         -e METHOD=$(METHOD) -e HOT_KEYS=$(HOT_KEYS) -e HOT_RATIO=$(HOT_RATIO)

up:
	docker compose up -d --build

down:
	docker compose down

load:
	docker compose run --rm -e MODE=constant $(K6_ENV) k6 run /scripts/load.js

ramp:
	docker compose run --rm -e MODE=ramp $(K6_ENV) k6 run /scripts/load.js

spike:
	docker compose run --rm -e MODE=spike $(K6_ENV) k6 run /scripts/load.js

slowloris:
	docker compose run --rm slowloris -conns $(CONNS) -duration $(DURATION)

vars:
	@curl -s localhost:6060/debug/vars | python3 -c 'import json,sys; d=json.load(sys.stdin); [print(f"{k:20} {json.dumps(d[k])}") for k in ("level3_config","db_queries","db_connects","db_errors","db_timeouts","db_last_error","cache_hits","cache_misses","cache_errors","singleflight_shared","db_pool") if k in d]'

pgconns:
	@docker compose exec -T postgres psql -U app -d shop -c "SELECT coalesce(state, 'background') AS state, count(*) FROM pg_stat_activity WHERE datname = 'shop' GROUP BY 1 ORDER BY 2 DESC;"

psql:
	docker compose exec postgres psql -U app -d shop

db-freeze:
	docker compose pause postgres && sleep $(FREEZE) && docker compose unpause postgres

db-reset:
	docker compose down -v && $(MAKE) up

stats:
	docker stats

logs:
	docker compose logs -f app

.PHONY: up down load ramp spike slowloris vars pgconns psql db-freeze db-reset stats logs
