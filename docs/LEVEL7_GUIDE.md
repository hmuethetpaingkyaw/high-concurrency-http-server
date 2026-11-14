# Level 7: Idempotency Key for Orders (simple guide)

## The idea in one minute

Sometimes an order is **saved**, but the customer never gets the reply (a database timeout right after the save, or a restart in the middle of the request). The customer's app does the sensible thing and **tries again**. Without protection, the second try creates a **second order** and takes the stock twice. Levels 5 and 6 each saw this.

**The fix:** the customer's app puts a unique code on each order, the `Idempotency-Key`. The server remembers the codes it has seen. A repeat with the same code gets the **first order back**, not a new one.

Kitchen version: every order slip has a ticket number. If the same ticket number comes in twice, the cook says "already made that one" and hands over the first dish.

```
 first try:   POST /orders  Idempotency-Key: abc  →  order saved, 201  (reply lost)
 second try:  POST /orders  Idempotency-Key: abc  →  "seen abc"  →  200 + the same order
```

## What the server does

| Situation | Answer |
| --- | --- |
| New key | `201`, order saved, stock down |
| Same key and same order again | `200` with the **same order id**, header `Idempotent-Replay: true`, nothing else changes |
| Same key, different order (other product or quantity) | `422`, nothing saved |
| Key is empty, over 128 characters, or has odd characters | `400` |
| Out of stock | `409`, and the key is **not** remembered, so the customer can try again later |
| No key at all | Works like before, no protection |

The key is saved **in the same transaction** as the order, in a new `orders.idempotency_key` column with a unique index. So an order and its key are saved together or not at all. Code: `createOrderWithKey` in [cmd/server/shop.go](../cmd/server/shop.go).

## The switch and the setup

| Switch | Default | What it does |
| --- | --- | --- |
| `IDEMPOTENCY` | `off` | `on` = use the `Idempotency-Key` header. `off` = the header is ignored (the problem, for the baseline) |

Setup, once (the database already exists):

```sh
LB_PORT=18080 make up      # LB_PORT only if port 8080 is taken on your Mac
make migrate               # adds the key column and index; safe to run twice
```

Pass all switches on every `make up`. `make vars` prints `level7_config` and `idempotent_replays`, so you can see whether the switch is on and how many replays happened.

## How to measure duplicates

The load client has two new options for `POST /orders`:

- `RETRY=on`: retry a failed request up to 2 more times (network error or `5xx`).
- `KEY=on`: send an `Idempotency-Key`, one per customer request, the same on every retry.

k6 prints three new numbers: `order_requests` (customer requests), `order_retries` (extra attempts) and `order_ok` (requests that ended in `2xx`).

Count the orders before and after the run:

```sh
docker compose exec -T postgres psql -U app -d shop -tAc "SELECT count(*) FROM orders"
```

**extra = orders added − `order_ok`.** With no protection, retries add duplicates, so extra grows. With protection, extra only counts orders that were saved while the customer finally gave up. Those are not duplicates.

---

## Step 0: The problem (switch off)

```sh
make up                                    # IDEMPOTENCY is off
```

**A. By hand:** the same order twice.

```sh
K=demo-$(date +%s)
curl -s -X POST localhost:18080/orders -H "Idempotency-Key: $K" -d '{"product_id":99991,"qty":1}'
curl -s -X POST localhost:18080/orders -H "Idempotency-Key: $K" -d '{"product_id":99991,"qty":1}'
docker compose exec -T postgres psql -U app -d shop -tAc "SELECT count(*) FROM orders WHERE product_id=99991"   # before and after: +2
```

**B. Under load:** a retrying client while the database freezes.

```sh
# terminal 1
make load ENDPOINT=/orders METHOD=POST RATE=300 DURATION=40s RETRY=on KEY=on
# terminal 2, about 10 s later
make db-freeze FREEZE=10
```

| Run | Orders added | `order_requests` | `order_ok` | `order_retries` | **extra** |
| --- | --- | --- | --- | --- | --- |
| switch off | | | | | |

**Question:** where do the extra orders come from? (A request is saved, the reply times out, the client retries, and the retry is saved too.)

## Step 1: Switch on

```sh
IDEMPOTENCY=on make up
```

By hand:

```sh
K=demo-$(date +%s)
curl -si -X POST localhost:18080/orders -H "Idempotency-Key: $K" -d '{"product_id":99992,"qty":2}'   # 201
curl -si -X POST localhost:18080/orders -H "Idempotency-Key: $K" -d '{"product_id":99992,"qty":2}'   # 200, Idempotent-Replay: true, same order_id
curl -si -X POST localhost:18080/orders -H "Idempotency-Key: $K" -d '{"product_id":99992,"qty":3}'   # 422
# 20 at the same time, spread over the 3 instances:
curl --parallel --parallel-max 20 -s -o /dev/null -w '%{http_code}\n' -H "Idempotency-Key: par-$K" -d '{"product_id":99994,"qty":1}' $(for i in $(seq 20); do printf 'localhost:18080/orders '; done) | sort | uniq -c
```

| Test | Expected | Seen |
| --- | --- | --- |
| Same key twice | `201`, then `200` with the same order id | |
| Same key, other quantity | `422` | |
| 20 at the same time | one `201`, nineteen `200`; one order in the table | |
| `idempotent_replays` (add the three instances: ports 6061-6063) | 19 more after the parallel test | |

## Step 2: Retries under failures

Same retrying load as step 0, now with the switch **on**, and a second kind of failure: a restart in the middle of requests (graceful shutdown is off here, so requests are cut off).

```sh
IDEMPOTENCY=on make up
# terminal 1
make load ENDPOINT=/orders METHOD=POST RATE=300 DURATION=40s RETRY=on KEY=on
# terminal 2, about 10 s later: A) a database freeze  (run again with B) instead)
make db-freeze FREEZE=10
make rolling-restart GAP=10                # B) restart the 3 instances one by one
```

Do the same two failures with `make up` (switch off) for comparison. Run each twice.

| Failure | Switch | Orders added | `order_ok` | `order_retries` | **extra** |
| --- | --- | --- | --- | --- | --- |
| DB freeze | off | | | | |
| DB freeze | on | | | | |
| Rolling restart | off | | | | |
| Rolling restart | on | | | | |

**Question:** with the switch on, is `extra` smaller? Can it still be above 0? (Yes: an order saved while the client finally gave up. That is not a duplicate.)

## Step 3: What does it cost?

Same load with the key sent in both runs, so the only difference is whether the server uses it.

```sh
make up                                    # off
make load ENDPOINT=/orders METHOD=POST RATE=300 DURATION=60s KEY=on
IDEMPOTENCY=on make up
make load ENDPOINT=/orders METHOD=POST RATE=300 DURATION=60s KEY=on
```

Repeat at `RATE=1000`. Note: `POST /orders` picks hot products 80% of the time, so many orders fight for the same stock row (the hot-row effect from Level 3). That can cap the rate for reasons that have nothing to do with the key.

| RATE | Switch | p99 | Errors | Postgres CPU |
| --- | --- | --- | --- | --- |
| 300 | off | | | |
| 300 | on | | | |
| 1000 | off | | | |
| 1000 | on | | | |

**Question:** the key adds a unique index and, on a repeat, one more lookup. Is that visible?

---

## Not solved here

- Keys are kept **forever**. A real system deletes them after a day or so.
- The server only compares product and quantity on a repeat, not the whole request.
- Only `POST /orders` is protected.
- A client that sends no key is not protected.

## Common mistakes

- Forgetting `IDEMPOTENCY=on` on `make up`: the header is then ignored and duplicates still happen.
- Reusing the same key for a new order by hand: you get the old order back, or a `422`. Use a fresh key per order.
- `make migrate` is needed once on an existing database. A fresh `make db-reset` already has the column.
