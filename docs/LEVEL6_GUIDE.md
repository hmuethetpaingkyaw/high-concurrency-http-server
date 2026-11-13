# Level 6: Graceful Shutdown (simple guide)

## The idea in one minute

When you **redeploy**, each app instance is stopped and started again. If it just dies, the customers it was serving get errors.

**Goal:** restart all three instances, one at a time, while traffic keeps flowing, with **no errors and no lost orders**.

Kitchen version: a waiter going off shift should (1) put up a "not taking new tables" sign, (2) finish the tables they already have, (3) then go home. Not just vanish.

How traffic flows (the load balancer asks every app `GET /readyz` once a second):

```
   customers (k6)
        │
        ▼
 ┌───────────────┐
 │   HAProxy     │── every 1 s: "GET /readyz?" ──┐
 │  (the host)   │                               │
 └───┬───┬───┬───┘                               │
     ▼   ▼   ▼                                   │
   app1 app2 app3  ◀─────────────────────────────┘
     │    │    │
     └────┴────┴──▶  Postgres + Redis (shared)
```

## The 3-step shutdown

| Step | What the app does | In the code |
| --- | --- | --- |
| 1 | Says "I'm not ready". The load balancer stops sending new requests. | `/readyz` starts answering `503` |
| 2 | Finishes the requests it already has. | `srv.Shutdown(...)` waits for them |
| 3 | Closes the database and Redis connections, then exits. | `shop.close()` |

Code: [cmd/server/main.go](../cmd/server/main.go). The load balancer reads `/readyz` through the `LB_HEALTH_PATH` switch in [lb/haproxy.cfg](../lb/haproxy.cfg).

## The switches

Everything is **off by default**, so you measure the problem first.

| Switch | Default | What it does |
| --- | --- | --- |
| `GRACEFUL_SHUTDOWN` | `off` | `on` = use the 3-step shutdown above |
| `DRAIN_DELAY` | `5s` | Time the app keeps serving after saying "not ready" |
| `SHUTDOWN_TIMEOUT` | `15s` | Longest wait for running requests, then force-close |
| `POOL_CLOSE_TIMEOUT` | `3s` | Longest wait for the database pool to close, then give up |
| `LB_HEALTH_PATH` | `/healthz` | `/readyz` = the load balancer notices the "not ready" sign |
| `STOP_GRACE_PERIOD` | `30s` | How long Docker waits before it kills the process. Must be longer than `DRAIN_DELAY` + `SHUTDOWN_TIMEOUT` |

Pass **all** the switches you want on **every** `make up`. Any you leave out go back to the default.

If port 8080 is used by another project on your Mac, add `LB_PORT=18080` to every `make up`. The load tests are not affected.

## The three timers

They run one after another on the same clock, which starts when the stop signal arrives (defaults shown).

```
 0 s            5 s                          20 s            30 s
 │──────────────│────────────────────────────│───────────────│
 │ DRAIN_DELAY  │ SHUTDOWN_TIMEOUT           │  spare time   │
 │ "sign up,    │ "finish the tables         │               │
 │  keep        │  you already have"         │               ▼
 │  serving"    │                            │         STOP_GRACE_PERIOD ends:
 │              │                            │         Docker kills the app
 ▼              ▼                            ▼
 not ready      server closes                latest the server
 (/readyz 503)  to new requests              force-closes stuck requests
```

| Timer | Set by | Meaning (kitchen version) |
| --- | --- | --- |
| `DRAIN_DELAY` (5 s) | the app | The waiter puts up the "not taking new tables" sign but keeps working for 5 s. This gives the host (the load balancer) time to notice. |
| `SHUTDOWN_TIMEOUT` (15 s) | the app | The most time the waiter gets to finish the tables they already have. After that, the rest are cut off. |
| `STOP_GRACE_PERIOD` (30 s) | Docker | The boss's hard deadline. If the waiter is still there at 30 s, they are thrown out (`SIGKILL`). |

**The rule:** `DRAIN_DELAY` + `SHUTDOWN_TIMEOUT` + `POOL_CLOSE_TIMEOUT` (5 + 15 + 3 = 23 s) must fit inside `STOP_GRACE_PERIOD` (30 s).

**Usually** the requests finish quickly, so the app exits at about 5.3 s. The 15 s is only a limit, not a wait.

**In step 3** the database was frozen. The app force-closed the stuck requests at the timeout, then closing the pool waited for the database. Before the fix that wait had no limit (20.6 s, and longer for a longer freeze). Now it gives up after `POOL_CLOSE_TIMEOUT`.

## How to run a redeploy test

Two terminals.

```sh
# terminal 1: steady traffic
make load ENDPOINT='/products/{id}' RATE=3000 DURATION=60s

# terminal 2, about 10 s later: restart the 3 instances one by one
make rolling-restart GAP=10
make lbstats                    # errors per instance
```

For **orders**, count the orders before and after, and compare with the `201` replies k6 got:

```sh
docker compose exec -T postgres psql -U app -d shop -tAc "SELECT count(*) FROM orders"
make load ENDPOINT=/orders METHOD=POST RATE=300 DURATION=60s     # restart during the run
docker compose exec -T postgres psql -U app -d shop -tAc "SELECT count(*) FROM orders"
```

**Extra orders** = orders added minus `201` replies. A positive number means an order was saved but the customer never got the reply. (k6 shows `unexpected 100%` for POST because it returns `201`. Ignore that line.)

---

## Results at a glance (run 1 / run 2)

| Step | What changed | GET: failed requests (3,000 RPS) | POST: failed requests (300 RPS) | POST: extra orders |
| --- | --- | --- | --- | --- |
| 0 | Nothing (naive) | 6 / 0 | 3 / 19 | 0 / +1 |
| 1 | App handles the stop signal | 0 / 0 | 2 / 451 | +1 / +74 |
| 2 | Plus the "not ready" sign (`/readyz`) | 0 / 0 | 0 / 4 | 0 / 0 |

**In plain words:** step 2 is the cleanest, but steps 0 and 1 were also close to zero for GET. The difference is **smaller than the run-to-run noise**, so these tests cannot prove which part helped.

## Step 0: No graceful shutdown (the baseline)

```sh
make up
```

Then run the redeploy test (GET, then POST).

**What we saw:** only a few failures (6 and 0 GET requests out of about 180,000). The Level 5 kill (about 1,160 dropped) did **not** repeat. A restart is gentler than a kill, because the load balancer was already routing around the instance.

## Step 1: Handle the stop signal only

```sh
GRACEFUL_SHUTDOWN=on DRAIN_DELAY=0s make up
```

The app now finishes its running requests, but it does not say "not ready" first.

**What we saw:** GET was clean (0 and 0). POST run 2 had 451 failures (2.51%) and 74 extra orders. Those failures were spread over all three instances, which looks like database timeouts (the Level 5 pattern), not the restart. It was not checked.

## Step 2: Add the "not ready" sign (the real fix)

```sh
GRACEFUL_SHUTDOWN=on LB_HEALTH_PATH=/readyz make up
```

To watch one instance drain (about 6 seconds):

```sh
docker compose stop app1        # terminal 1
make lbstats                    # terminal 2, repeat: app1 goes DOWN while it still serves
docker compose logs app1 | grep shutdown
```

**What we saw:**
- The load balancer marked `app1` DOWN after **1.4 to 2 s**. The app kept serving for the full 5 s, then exited. `docker stop` returned after about **5.3 s**.
- Redeploy test: 0 failed GETs in both runs, and 0 and 4 failed POSTs. **No extra orders.**
- Run 1 of GET dropped 382 requests but had no errors. Most likely noise: run 2 dropped 0.

## Step 3: A request that outlasts the time limit

What if something is still running when time is up? Freeze the database so requests hang:

```sh
GRACEFUL_SHUTDOWN=on LB_HEALTH_PATH=/readyz SHUTDOWN_TIMEOUT=8s DB_TIMEOUT=0 CACHE=off make up
make load ENDPOINT='/products/{id}' RATE=200 DURATION=45s      # terminal 1
make db-freeze FREEZE=25 &                                      # terminal 2
sleep 5; time docker compose restart app1
docker compose logs app1 | grep shutdown
```

**What we saw:**
- After 8 s the server gave up waiting and force-closed the hung requests (`context deadline exceeded`).
- **Before the fix** the restart took **20.6 s**, not 13 s. The last step (`shop.close()`) waited for the frozen database and only finished when Postgres came back. That was a bug in my code: closing the pool had no time limit, so a longer freeze would have been killed by Docker.
- **After the fix** (`POOL_CLOSE_TIMEOUT=3s`) the same test took **16.4 s** (5 s drain + 8 s timeout + 3 s pool). The log shows `pool still has busy connections after 3s, giving up`. The wait no longer depends on how long the database stays frozen.
- The 27.7% failed requests came from the frozen database (all three instances hung), not from the restart.

## Step 4: Heavier load

```sh
GRACEFUL_SHUTDOWN=on LB_HEALTH_PATH=/readyz make up
make load ENDPOINT='/products/{id}' RATE=5000 DURATION=60s     # rolling-restart in terminal 2
```

| RATE | Run 1 | Run 2 |
| --- | --- | --- |
| 5,000 | pass (0 errors) | pass (0 errors) |
| 10,000 | **fail** (0.41% errors) | pass (0 errors) |

At 10,000 RPS the load tool (k6) is near its own limit. Level 5 failed 2 of 4 runs at 10,000 **without any restart**, so this is no better or worse.

---

## What we learned

1. The 3-step shutdown works as designed: not ready, then finish, then exit (about 5 s).
2. In this lab the **benefit is small**, because the naive restart was already gentle.
3. Step 2 gave the fewest POST failures and no extra orders, but the gap is inside the noise.
4. Closing the pool needs a time limit (found in step 3, fixed with `POOL_CLOSE_TIMEOUT`).

## Still not solved

- **Saved but unanswered orders** can also come from database timeouts (Level 5 saw 55 extra). Graceful shutdown does not stop that. An **idempotency key** on `POST /orders` would.
- A harsher baseline test (`docker kill` instead of a restart) would show the benefit more clearly.

## Common mistakes

- `docker compose up -d app1` without switches recreates the instance with **default** settings. Use `make up` with all your switches.
- If `LB_HEALTH_PATH` stays `/healthz`, the "not ready" sign is ignored.
- `docker compose restart` waits up to `STOP_GRACE_PERIOD` (30 s). Docker's own default of 10 s is too short for a 5 s drain plus shutdown.
