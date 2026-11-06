# Level 3 Test Guide: Postgres + Redis

Level 3 puts a real database and cache behind the server. The code for every fix is already written, and each fix is an **on/off switch** (an environment variable). The defaults are the naive version, so you start from the failure and turn on one fix at a time.

Record your numbers in the tables at each step, then send them to Claude to log in `LAB_LOG.md`.

## What's new

| Piece | Size (EC2-style) | Notes |
| --- | --- | --- |
| Postgres 16 | 1 CPU / 1 GB (~db.t4g.small) | 100,000 products in 50 categories. `max_connections` = 100 (default) |
| Redis 7 | 0.5 CPU / 256 MB (~cache.t4g.micro) | No persistence, 200 MB max, evicts least-recently-used keys |
| App | 0.5 CPU / 512 MB (unchanged) | Code in [cmd/server/shop.go](../cmd/server/shop.go) |

| Endpoint | What it does | k6 `ENDPOINT` |
| --- | --- | --- |
| `GET /products/{id}` | One product + stock (read-heavy, cacheable) | `'/products/{id}'` |
| `GET /products?category=cat-7&limit=20` | A page of 20 products + their stock | `'/products?category={cat}'` |
| `POST /orders` `{"product_id": 42, "qty": 1}` | Transaction: decrement stock, insert order | `/orders METHOD=POST` |

k6 picks the ids: `{id}` sends 80% of requests to the 1,000 most popular products and 20% to all 100,000 (change with `HOT_KEYS` and `HOT_RATIO`). `{cat}` picks a random category. **Always quote** endpoints with `{` or `?` in zsh.

## The switches

| Step | Switch | Off (naive, default) | On (fixed) |
| --- | --- | --- | --- |
| 1 | `DB_POOL=on` (+ `DB_MAX_CONNS`, default 10) | New connection for every request | Reuse a pool of connections |
| 2 | `DB_TIMEOUT=300ms` | `context.Background()`: DB work never gives up | Deadline tied to the request; `504` when it passes |
| 3 | `LIST_QUERY=join` | N+1: 21 queries per page of 20 | 1 query with a JOIN |
| 4 | `CACHE=on` (+ `CACHE_TTL`, default 30s) | Every read goes to Postgres | Redis cache-aside for `GET /products/{id}` |
| 5 | `SINGLEFLIGHT=on` | Every cache miss queries Postgres | Concurrent misses for one key share one query |

Switches are read at startup. **Pass all the switches you want on every time you run `make up`.** Any you leave out go back to off:

```sh
make up                                                    # step 0: everything naive
DB_POOL=on make up                                         # step 1
DB_POOL=on DB_TIMEOUT=300ms make up                        # step 2
DB_POOL=on DB_TIMEOUT=300ms LIST_QUERY=join make up        # step 3
DB_POOL=on DB_TIMEOUT=300ms LIST_QUERY=join CACHE=on make up                   # step 4
DB_POOL=on DB_TIMEOUT=300ms LIST_QUERY=join CACHE=on SINGLEFLIGHT=on make up   # step 5
```

`make vars` prints `level3_config`, so you can always check which switches are on.

## Setup (once)

```sh
git checkout work/level-3
docker stop db sfa_api-localstack-1    # free CPU from other projects
make up                                # first start seeds 100k products (~10-30 s)
curl -s localhost:8080/products/42     # should print a product
make vars                              # all switches off, counters near 0
```

Open **3 terminals**:

| Terminal | Command | Watch for |
| --- | --- | --- |
| 1 | the test commands below | k6 results |
| 2 | `make stats` | CPU and memory of **app, postgres and redis** |
| 3 | `while true; do clear; make -s vars; make -s pgconns; sleep 2; done` | DB queries, new connections, errors, cache hits, pool, Postgres connection count |

Counters reset whenever the app restarts (`make up`). Empty Redis with `docker compose exec redis redis-cli FLUSHALL`. Start over with fresh data using `make db-reset`.

---

## Step 0: Naive baseline (failure first)

```sh
make up
make load ENDPOINT='/products/{id}' RATE=100 DURATION=30s
make load ENDPOINT='/products/{id}' RATE=300 DURATION=30s
make load ENDPOINT='/products/{id}' RATE=600 DURATION=30s
```

**Watch:**
- `db_connects` rises by one **per request**. Every request opens and closes its own connection.
- `make pgconns`: how close does the count get to Postgres's limit of 100?
- `db_errors` and `db_last_error`: look for `sorry, too many clients already` or `connection reset by peer`. `db_last_error` only keeps the most recent error. To count how often Postgres refused connections, run `docker compose logs postgres | grep -c "too many clients"`.
- Postgres CPU in `make stats`. Opening a connection costs Postgres a new process plus a password check.

Also measure the other two endpoints:
```sh
make load ENDPOINT='/products?category={cat}' RATE=100 DURATION=30s
make load ENDPOINT=/orders METHOD=POST RATE=100 DURATION=30s
```

| Endpoint | RATE | p99 | Errors | `db_connects` | Max Postgres connections | `db_last_error` | Pass? |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `/products/{id}` | 100 | | | | | | |
| `/products/{id}` | 300 | | | | | | |
| `/products/{id}` | 600 | | | | | | |
| list | 100 | | | | | | |
| orders | 100 | | | | | | |

**Questions:** At what rate do errors start? Is the bottleneck the app's CPU, Postgres's CPU, or the connection limit?

---

## Step 1: Connection pool

```sh
DB_POOL=on make up
make load ENDPOINT='/products/{id}' RATE=600 DURATION=30s     # same rate that failed in step 0
```

**Watch:** `db_connects` should stop at about `DB_MAX_CONNS`, and `make pgconns` should stay at or below the pool size.

**Now size the pool.** Hold one rate and change only the pool size:
```sh
for n in 2 5 10 20 50 90; do
  DB_POOL=on DB_MAX_CONNS=$n make up
  make load ENDPOINT='/products/{id}' RATE=1500 DURATION=30s
  make vars | grep db_pool
done
```

| `DB_MAX_CONNS` | p99 | Errors | Dropped | `had_to_wait` | `wait_total_ms` | Postgres CPU |
| --- | --- | --- | --- | --- | --- | --- |
| 2 | | | | | | |
| 5 | | | | | | |
| 10 | | | | | | |
| 20 | | | | | | |
| 50 | | | | | | |
| 90 | | | | | | |

**How to read it:**
- `had_to_wait` counts how many times a request found no free connection and had to wait. `wait_total_ms` is the total time spent waiting.
- **Too small:** requests queue for a connection, so `wait_total_ms` climbs.
- **Too big:** Postgres has only 1 CPU and switches between dozens of queries, so each one gets slower.
- **Little's Law:** connections needed = queries per second × query time. For example, 1,500/s × 2 ms = 3 connections busy on average. Look for where p99 is best.

Then find the ceiling with your best pool size: try 1,500, then 3,000, then halve the gap.

---

## Step 2: Query timeouts (a stuck database)

`make db-freeze FREEZE=10` freezes Postgres for 10 seconds. That's what a failover, a long lock or a network problem looks like to the app.

Run each case in **two terminals**. Start the load first, then the freeze about 15 seconds later:
```sh
# Case A: no timeout
DB_POOL=on make up
make load ENDPOINT='/products/{id}' RATE=300 DURATION=60s     # terminal 1
make db-freeze FREEZE=10                                      # terminal 4, ~15 s in

# Case B: with timeout
DB_POOL=on DB_TIMEOUT=300ms make up
make load ENDPOINT='/products/{id}' RATE=300 DURATION=60s
make db-freeze FREEZE=10
```

**Watch** during the freeze:
- Goroutines: `curl -s 'localhost:6060/debug/pprof/goroutine?debug=1' | head -1`
- App memory in `make stats`
- `db_timeouts`
- How quickly things recover after the unfreeze

| Case | Errors | Status codes during the freeze | Peak goroutines | Peak memory | Time to recover |
| --- | --- | --- | --- | --- | --- |
| A: no timeout | | | | | |
| B: `DB_TIMEOUT=300ms` | | | | | |

**What to understand:** without a timeout, requests wait until the database returns, and the in-flight cap (`MAX_INFLIGHT_DB`) fills up. With a timeout, users get a quick `504` and the server stays light. The naive code also uses `context.Background()`, so DB work continues even after the client has hung up.

---

## Step 3: Fix N+1

```sh
DB_POOL=on DB_TIMEOUT=300ms make up                        # still N+1
make load ENDPOINT='/products?category={cat}' RATE=300 DURATION=30s
make vars | grep db_queries

DB_POOL=on DB_TIMEOUT=300ms LIST_QUERY=join make up
make load ENDPOINT='/products?category={cat}' RATE=300 DURATION=30s
make vars | grep db_queries
```

| `LIST_QUERY` | Queries per request (`db_queries` ÷ requests) | p99 | Postgres CPU | Ceiling |
| --- | --- | --- | --- | --- |
| n+1 | | | | |
| join | | | | |

**Question:** each extra query is a network round trip plus work for Postgres. How much of the difference is round trips, and how much is Postgres CPU?

---

## Step 4: Redis cache

```sh
docker compose exec redis redis-cli FLUSHALL
DB_POOL=on DB_TIMEOUT=300ms LIST_QUERY=join CACHE=on make up
make load ENDPOINT='/products/{id}' RATE=1500 DURATION=60s
make vars | grep -E "cache_hits|cache_misses|db_queries"
```

**Hit ratio** = `cache_hits` ÷ (`cache_hits` + `cache_misses`).

Find the new ceiling (try 3,000, 5,000, 8,000), then see how much the hit ratio depends on the traffic pattern:
```sh
make load ENDPOINT='/products/{id}' RATE=3000 DURATION=60s HOT_RATIO=0      # uniform: every product equally likely
make load ENDPOINT='/products/{id}' RATE=3000 DURATION=60s HOT_RATIO=0.8    # default skew
make load ENDPOINT='/products/{id}' RATE=3000 DURATION=60s HOT_RATIO=0.99   # very skewed
```

| `HOT_RATIO` | RATE | Hit ratio | p99 | Postgres CPU | Pass? |
| --- | --- | --- | --- | --- | --- |
| 0 | 3000 | | | | |
| 0.8 | 3000 | | | | |
| 0.99 | 3000 | | | | |

**Check invalidation:** an order should remove the cached product, so the next read shows the new stock:
```sh
curl -s localhost:8080/products/7 | python3 -c 'import json,sys;print(json.load(sys.stdin)["stock"])'
curl -s -X POST localhost:8080/orders -d '{"product_id":7,"qty":5}'
curl -s localhost:8080/products/7 | python3 -c 'import json,sys;print(json.load(sys.stdin)["stock"])'   # 5 lower
```

**Level 3 target:** `GET /products/{id}` passes the SLO at **5,000 RPS** on the 0.5 CPU app.

---

## Step 5: Cache stampede and singleflight

A stampede happens when a hot key expires and hundreds of requests miss at the same moment. Make one very hot key that expires every second:

```sh
# Without singleflight
docker compose exec redis redis-cli FLUSHALL
DB_POOL=on DB_TIMEOUT=300ms LIST_QUERY=join CACHE=on CACHE_TTL=1s make up
make load ENDPOINT='/products/{id}' RATE=3000 DURATION=30s HOT_KEYS=1 HOT_RATIO=1
make vars | grep -E "db_queries|cache_misses|singleflight"

# With singleflight
docker compose exec redis redis-cli FLUSHALL
DB_POOL=on DB_TIMEOUT=300ms LIST_QUERY=join CACHE=on CACHE_TTL=1s SINGLEFLIGHT=on make up
make load ENDPOINT='/products/{id}' RATE=3000 DURATION=30s HOT_KEYS=1 HOT_RATIO=1
make vars | grep -E "db_queries|cache_misses|singleflight"
```

You can also see it in 1 second with curl, no k6 needed. This sends 300 requests at once for one key that isn't cached:
```sh
docker compose exec redis redis-cli FLUSHALL
curl -s -o /dev/null --parallel --parallel-immediate --parallel-max 300 "localhost:8080/products/9?[1-300]"
make vars | grep -E "db_queries|singleflight"
# Without singleflight: db_queries rises by about 300. With it: 1.
```

| `SINGLEFLIGHT` | `db_queries` in 30 s | Queries per expiry (÷ 30) | p99 |
| --- | --- | --- | --- |
| off | | | |
| on | | | |

---

## Bonus: hot rows in orders

Every order runs `UPDATE inventory ... WHERE product_id = $1`, which **locks that row** until the transaction commits. Orders for the same product therefore run one at a time. It's Level 0's global lock again, this time inside the database.

```sh
DB_POOL=on DB_MAX_CONNS=20 DB_TIMEOUT=300ms make up
make load ENDPOINT=/orders METHOD=POST RATE=300 DURATION=30s HOT_KEYS=1000 HOT_RATIO=0.8   # spread out
make load ENDPOINT=/orders METHOD=POST RATE=300 DURATION=30s HOT_KEYS=1 HOT_RATIO=1        # one product
```

During the second run, see who is waiting on a lock:
```sh
docker compose exec -T postgres psql -U app -d shop -c \
  "SELECT wait_event_type, wait_event, count(*) FROM pg_stat_activity WHERE datname='shop' GROUP BY 1,2 ORDER BY 3 DESC;"
```

| Orders | p99 | Errors | Rows waiting on `Lock` |
| --- | --- | --- | --- |
| Spread over 1,000 products | | | |
| All on 1 product | | | |

---

## Reference

| Command | What it does |
| --- | --- |
| `make vars` | App counters: switches, queries, new connections, errors, last error, cache, singleflight, pool |
| `make pgconns` | Connections Postgres sees, grouped by state |
| `make psql` | Postgres shell |
| `make db-freeze FREEZE=10` | Freeze Postgres for N seconds |
| `make db-reset` | Delete the database volume and re-seed |
| `MAX_INFLIGHT_DB=…` | In-flight cap for each shop endpoint (default 1,000) |

**Pitfalls:**
- **zsh:** quote endpoints that contain `?` or `{`: `ENDPOINT='/products?category={cat}'`.
- **Switches:** they reset on every `make up`. Check `level3_config` in `make vars`.
- **CPU budget:** k6 3 + app 0.5 + Postgres 1 + Redis 0.5 = 5 of Docker's 8 CPUs. Stop other projects' containers before testing.
- **k6 as the limit:** if k6 is near 300% CPU in `make stats`, the test tool is the bottleneck (F8).
