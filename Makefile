# Usage:
#   make up                                  build and start 3 app instances + load balancer
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
# Level 5 (see docs/LEVEL5_GUIDE.md):
#   make up N=1                              run only app1 behind the load balancer
#   make lbstats                             per-instance status, requests, errors (from HAProxy)
#   make vars I=2                            counters of instance 2 (pprof/expvar on ports 6061-6063)
#   make kill-app I=2 / make start-app I=2   kill or restart one instance
#   make slow-app I=2 / make fast-app I=2    throttle one instance to 0.1 CPU / restore 0.5
ENDPOINT ?= /io
RATE     ?= 100
DURATION ?= 1m
CONNS    ?= 1000
METHOD   ?= GET
HOT_KEYS ?= 1000
HOT_RATIO ?= 0.8
FREEZE   ?= 10
N        ?= 3
I        ?= 1
TARGET   ?= http://lb:8080

# Quoted: endpoints like /products?category={cat} contain shell glob characters.
K6_ENV = -e ENDPOINT='$(ENDPOINT)' -e RATE=$(RATE) -e DURATION=$(DURATION) \
         -e METHOD=$(METHOD) -e HOT_KEYS=$(HOT_KEYS) -e HOT_RATIO=$(HOT_RATIO) \
         -e TARGET=$(TARGET)

# N instances (1-3) behind the load balancer. Instances above N are stopped.
up:
	docker compose up -d --build --remove-orphans lb $(wordlist 1,$(N),app1 app2 app3)
	@for i in 1 2 3; do if [ $$i -gt $(N) ]; then docker compose stop app$$i >/dev/null 2>&1; fi; done

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
	@curl -s localhost:606$(I)/debug/vars | python3 -c 'import json,sys; d=json.load(sys.stdin); [print(f"{k:20} {json.dumps(d[k])}") for k in ("level3_config","db_queries","db_connects","db_errors","db_timeouts","db_last_error","cache_hits","cache_misses","cache_errors","singleflight_shared","db_pool") if k in d]'

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
	docker compose logs -f app1 app2 app3 lb

lbstats:
	@python3 lb/stats.py

kill-app:
	docker compose kill app$(I)

start-app:
	docker compose up -d app$(I)

slow-app:
	docker update --cpus 0.1 $$(docker compose ps -q app$(I))

fast-app:
	docker update --cpus 0.5 $$(docker compose ps -q app$(I))

.PHONY: up down load ramp spike slowloris vars pgconns psql db-freeze db-reset stats logs lbstats kill-app start-app slow-app fast-app
