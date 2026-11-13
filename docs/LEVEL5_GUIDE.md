# Level 5 Test Guide: 3 instances behind a load balancer

Level 5 puts HAProxy in front of **three** copies of the app. The setup is already built. You run the tests, fill in the tables, and send the numbers to Claude to log in `LAB_LOG.md`.

**Goal:** `GET /products/{id}` passes the SLO at **10,000 RPS** through the load balancer. A single instance topped out at ~5,000 RPS. Also: survive an instance dying under load, and shed overload at the edge (F7).

**SLO:** p99 < 200 ms, errors < 0.1%, 0 dropped iterations. (Spike runs use the spike SLO from the lab log.)

## What's new

| Piece | Size | Notes |
| --- | --- | --- |
| `lb` (HAProxy 3.0) | 1 CPU / 256 MB | Only public entry: `localhost:8080`. Stats page: http://localhost:8404/stats. Config: [lb/haproxy.cfg](../lb/haproxy.cfg) |
| `app1`, `app2`, `app3` | 0.5 CPU / 512 MB each (the Level 3 box) | Reached only through `lb`. pprof/expvar on `127.0.0.1:6061`, `6062`, `6063` |
| Postgres, Redis | unchanged (1 CPU, 0.5 CPU) | Now shared by all three instances: 3 pools x 10 connections = **30 of 100** |
| k6 | 3 CPU / 3 GB | Now targets `http://lb:8080`. `TARGET=http://app1:8080` bypasses the load balancer |

CPU budget: k6 3 + Postgres 1 + Redis 0.5 + 3 apps 1.5 + lb 1 = **7 of the VM's 8 CPUs**. It's tight: expect more noise, run each test twice.

**The Level 3 switches now default to the fixed settings** (pool on, `DB_TIMEOUT=300ms`, join, cache on, singleflight on). To reproduce a naive step, turn it off, for example `DB_POOL=off make up`.

## Commands

```sh
make up                      # 3 instances + load balancer
make up N=1                  # only app1 behind the load balancer (instances above N are stopped)
make lbstats                 # per instance: status, in flight, total, 2xx, 5xx, failed connects, retries, queue
make vars I=2                # app counters of instance 2 (db_pool, cache_hits, ...)
make kill-app I=2            # kill instance 2          make start-app I=2   # start it again
make slow-app I=2            # limit instance 2 to 0.1 CPU     make fast-app I=2   # back to 0.5
curl -si localhost:8080/products/7 | grep -i x-served-by      # which instance answered
```

`make lbstats` counts since the load balancer started. To reset the counters: `docker compose restart lb`.

---

## Step 0: The load balancer's own cost

One instance, with and without the load balancer in front. Level 4's baseline (no load balancer, 3,000 RPS, 45 s): median 0.25 ms, p95 0.63 ms.

```sh
make up N=1
make load ENDPOINT='/products/{id}' RATE=3000 DURATION=45s TARGET=http://app1:8080   # direct, bypasses lb
make load ENDPOINT='/products/{id}' RATE=3000 DURATION=45s                           # through lb
docker stats --no-stream      # in a second terminal while a test runs
```

| Path | p50 | p95 | p99 | App CPU | lb CPU | Pass? |
| --- | --- | --- | --- | --- | --- | --- |
| Direct to `app1` | 0.25 / 0.25 ms | 0.64 / 0.58 ms | 3.44 / 1.04 ms | 35 / 37% | n/a | ✅ / ✅ |
| Through `lb` | 0.28 / 0.27 ms | 0.57 / 0.57 ms | 1.62 / 1.32 ms | 29 / 29% | 16 / 16% | ✅ / ✅ |

**Question:** how much latency and CPU does the load balancer add per request? (A load balancer that opened a new connection to the app for every request would show up here as high latency and CPU; HAProxy reuses connections.)

**Recorded results (2026-11-10).** Two runs each, 3,000 RPS, 45 s, one instance (`make up N=1`); values are run 1 / run 2. 0 errors and 0 dropped in all four runs. The load balancer adds about 25-30 microseconds to the median (0.28 / 0.27 ms vs 0.25 / 0.25 ms) and nothing visible at p95 (0.57 vs 0.64 / 0.58 ms). The p99 is noisy (direct 3.44 and 1.04 ms, lb 1.62 and 1.32 ms), so the two paths can't be told apart there. HAProxy used about 16% of one CPU at 3,000 RPS (roughly 50 microseconds of CPU per request). `app1` used 29% through the lb against 35-37% direct; I did not find out why it is lower behind HAProxy (fewer client connections to the app is a guess, not checked).


## Step 1: Three instances, round-robin

```sh
make up
docker compose restart lb          # reset the counters
make load ENDPOINT='/products/{id}' RATE=5000 DURATION=60s
make lbstats
make pgconns                       # how many connections does Postgres see?
```

Repeat at 8,000 and 10,000 (`RATE=...`). Run each twice.

| RATE | Achieved | p99 | Errors | Dropped | app1 / app2 / app3 CPU | lb CPU | Pass? |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 5000 | 4,999.8 / 4,999.7 | 1.80 / 1.45 ms | 0% / 0% | 0 / 0 | 19.5 / 19.5 / 19.5% | 24 / 23% | ✅ / ✅ |
| 8000 | 7,999 / 7,993 | 6.26 / 34.05 ms | 0% / 0% | 0 / 398 | 31.5 / 31.5 / 31.7% (run 1) | 38.5% | ✅ / ❌ (dropped 398) |
| 10000 | 9,996 / 9,999 | 7.94 / 8.86 ms | 0% / 0% | 177 / 0 | 37.5 / 37.5 / 37.6% (run 1) | 46.5% | ❌ (dropped 177) / ✅ |

| Check | Result |
| --- | --- |
| Requests per instance after a run (`stot` in `make lbstats`): even? | Yes. 10,000 RPS run 2: app1 200,001 / app2 200,000 / app3 200,001. 5,000 RPS run 1: 100,001 each |
| Postgres connections (`make pgconns`) | 30 idle + 1 active (the `psql` query itself) = 3 pools x 10, in every run |

**Question:** one instance did ~5,000. Did three do ~15,000 (3x)? If not, which part is the limit (see CPU numbers)?

**Recorded results (2026-11-10).** Two runs per rate, 60 s each, lb restarted before every run. Table values are run 1 / run 2. Per-instance CPU shown for run 1 (run 2 within 0.5 points). k6 itself used about 60% of a CPU at 5,000, 95% at 8,000 and 111% at 10,000 (limit 300%). Redis served about 5,800 / 9,200 / 11,350 ops/s at 5,000 / 8,000 / 10,000 RPS.

- **Throughput scaled.** Three instances carry 10,000 RPS with every app at about 37% of its 50% quota. The first dropped iterations appeared only at 8,000 and 10,000, and not consistently: 8,000 run 2 dropped 398, 10,000 run 1 dropped 177, the other two runs dropped 0. The two runs disagree on pass/fail at 8,000 and at 10,000, and the fail is only the dropped-iterations check (p99 and errors passed every time). No component was close to its CPU limit in these runs, so I can't name a cause for those drops.
- **Traffic is even**: `stot` per instance differs by 1 or 2 requests (8,000 RPS run 2: 159,867 / 159,868 / 159,868).
- **Postgres** shows 30 idle connections (3 pools x 10) plus the one `active` row that is the monitoring query.

**Answer to the question.** I could not measure "3x of one instance's ceiling": k6 caps near 10,000 RPS, and the system handled all of it, so the ceiling of three instances is above 10,000 and was not found. Nothing was at its CPU limit (apps 74% of quota, lb 47%, Redis 40%).


## Step 2: An instance dies under load

Two different failures, because the load balancer finds out in different ways:

- **Kill:** the container disappears, so Docker DNS stops resolving its name and HAProxy takes it out within a second or two.
- **Pause:** the container stays, but freezes (like a hung process). Only the health check notices (every 1 s, 2 failures needed), and requests sent in the meantime hang.

```sh
make up
docker compose restart lb
# terminal 1:
make load ENDPOINT='/products/{id}' RATE=3000 DURATION=60s
# terminal 2, about 15 s after the load starts:
make kill-app I=2        # then, about 20 s later:  make start-app I=2
make lbstats
```

Repeat with a pause instead: `docker compose pause app2`, then 20 s later `docker compose unpause app2`.

| Failure | Errors (k6 %) | Failed requests | p99 | Dropped | Time until the LB marks it DOWN | Recovered after restart? |
| --- | --- | --- | --- | --- | --- | --- |
| `kill` | 0% / 0% | 0 / 0 | 1 s / 1 s | 1,163 / 1,164 | 3.4 s / 3.4 s (`DOWN` once, `MAINT (resolution)` once) | Yes, UP 2.1 s after `start-app` |
| `pause` | 1.09% / 1.11% | 1,918 / 1,966 | 5 s / 5 s | 4,186 / 4,147 | 3.8 s / 3.8 s (`DOWN`) | Yes, UP 1.6 s after `unpause` |

**Retries (the dangerous case):** the load balancer retries only when the connection itself failed (the request was never sent), so `POST /orders` is never sent twice. Check it:

```sh
make psql      # then:  SELECT count(*) FROM orders;   (note it, then \q)
make load ENDPOINT=/orders METHOD=POST RATE=300 DURATION=40s     # kill app2 about 15 s in
make psql      # SELECT count(*) FROM orders;
```

| Run | Orders added | `201` responses (k6 `http_reqs` minus failures) | Duplicates? |
| --- | --- | --- | --- |
| a | 11,946 | 11,946 | No |
| b | 11,270 | 11,215 (11,918 minus 703 failed) | **55 more orders than `201`s**, not caused by an LB retry (see note) |
| c | 11,947 | 11,947 | No |

**Question:** can the order count be *higher* than the `201`s k6 saw? Why would a retry on a `POST` be dangerous? (An order could be created, the reply lost, and the retry creates a second one.) This is why Level 6 exists: draining an instance before it stops.

**Recorded results (2026-11-10).** 3,000 RPS for 60 s. The instance was killed or paused 15 s in and restored about 20 s after that. Values are run 1 / run 2 (all in the table above). Time to `DOWN` was polled every 0.5 s from the moment of the action.

- **Kill:** k6 saw **0 failed requests**, but p99 was 1 s in both runs and 1,163 / 1,164 iterations were dropped, so it fails the SLO on p99 and dropped (not on errors). A stalled request to the dead instance waits for the 1 s connect timeout and is sent to another instance. App2's `stot` ended 3,151 / 2,805 above its `2xx` count, the sessions that went to the dead instance. `make lbstats` shows `econ` and `wretr` as 0 even so (I did not find out why the retry counters stay at 0). The status polls went `UP`, `UP 1/2` (one failed check) at about 2.9 s, then `DOWN` (run 1) or `MAINT (resolution)` (run 2) at 3.4 s.
- **Pause:** **1.09% / 1.11% errors** (1,918 / 1,966 requests, p99 5 s, 4,186 / 4,147 dropped). Requests sent to the frozen instance hang until k6 gives up at 5 s. The health check needed 3.8 s to mark it `DOWN` (two 1 s failed checks plus its own timeout). Fails the SLO on all three checks.
- **Recovery:** the instance was `UP` again 2.1 s (kill) and 1.6 s (pause) after it was restored. The final `lbstats` show all three `UP`.

**POST /orders check** (300 RPS, 40 s, app2 killed at 15 s, restarted at 27 s, three runs). Runs a and c: 0 failed requests and orders added = `201`s exactly (11,946 and 11,947), so no duplicate. Run b was different: 703 requests failed with a 5xx, and 55 more orders were created (11,270) than the 11,215 `201`s k6 counted. The 703 failures were database timeouts (`DB_TIMEOUT=300ms`), spread over all three instances (`db_timeouts` 224 / 248 / 231 matches the 5xx counts per instance exactly), not caused by the kill. The load balancer did not retry anything (`econ` and `wretr` 0 on every row), so these extra orders did not come from an LB retry. The most likely cause is a request whose insert committed but whose reply was lost to the 300 ms timeout; I did not verify that. Also: k6 reports `unexpected` as 100% in the POST runs because the script treats anything except 200 and 503 as unexpected and `POST /orders` returns 201 (not 200 or 503); ignore that line.

**Answer to the question.** The order count can be higher than the `201`s: it was in run b (+55), though the cause was DB timeouts after commit, not an LB retry. That is the same danger as a retried POST: the work happens, the client sees a failure or a lost reply, and a second attempt would double it. The LB config only retries when the connection could not be made, which is why kill alone gave no duplicates.


## Step 3: One slow instance

```sh
make up
make slow-app I=2                  # instance 2 limited to 0.1 CPU
docker compose restart lb
make load ENDPOINT='/products/{id}' RATE=4500 DURATION=45s      # round-robin: each instance gets ~1,500 RPS
make lbstats
LB_BALANCE=leastconn docker compose up -d --no-deps lb          # route to the instance with the fewest requests in flight
make load ENDPOINT='/products/{id}' RATE=4500 DURATION=45s
make lbstats
make fast-app I=2                  # restore
```

| `LB_BALANCE` | p99 | Errors | Dropped | Requests to app2 (`stot`) | Pass? |
| --- | --- | --- | --- | --- | --- |
| roundrobin | 70.8 / 67.9 ms | 0% / 0% | 0 / 0 | 67,497 / 67,500 (one third) | ✅ / ✅ |
| leastconn | 34.2 / 32.0 ms | 0% / 0% | 1,649 / 0 | 40,819 / 43,234 (about 20%) | ❌ (dropped 1,649) / ✅ |

**Question:** round-robin sends app2 a third of the traffic even though it can only handle part of that. Why does that raise p99 for *everyone's* requests that land on it? How does `leastconn` notice that app2 is slow?

**Recorded results (2026-11-10).** 4,500 RPS, 45 s, app2 at 0.1 CPU, two runs per policy (run 1 / run 2).

- **round-robin:** p99 70.8 / 67.9 ms, 0 errors, 0 dropped, pass both. App2 got exactly a third (67,497 / 67,500). App2 sat at about 11% CPU (its limit is 10%), app1 and app3 at 17%. Median stayed 0.3 ms, but p90 was 33-34 ms: the third of requests that land on app2 queue behind each other.
- **leastconn:** p99 34.2 / 32.0 ms, 0 errors; dropped 1,649 in run 1 (fail) and 0 in run 2 (pass). App2 got about 20% of the traffic (40,819 / 43,234). p90 was 0.43-0.48 ms. The run-1 drops came with one k6 sample at 179% CPU and Postgres at 23%; I can't tell if that was noise.

**Answer to the question.** Round-robin did not break the SLO here (p99 about 70 ms, well under 200 ms), because the cached read is so cheap that even a 0.1 CPU instance serves 1,500 RPS; but its p99 is about twice that of `leastconn` (70 vs 33 ms). Requests that land on app2 wait in its queue, and p99 only needs 1% of requests to be slow while a third land there. `leastconn` notices because app2 finishes requests slowly, so its in-flight count stays higher and the balancer sends new requests to the others, giving it about 20% instead of 33%.


## Step 4: Shed overload at the edge (F7)

One instance, overloaded at 2x its capacity (k6 tops out near 10,000 RPS, so use one instance). Level 2 found that shedding *inside* a box that is already out of CPU can't help (F7). Here the load balancer rejects the excess before it reaches the app.

```sh
make up N=1
# a) shedding only in the app (in-flight cap 1,000, the Level 2 setting)
make spike ENDPOINT='/products/{id}' RATE=10000 DURATION=30s
# b) shedding at the load balancer: at most 200 in flight per instance, wait up to 100 ms, then 503
LB_SERVER_MAXCONN=200 LB_QUEUE_TIMEOUT=100ms docker compose up -d --no-deps lb
make spike ENDPOINT='/products/{id}' RATE=10000 DURATION=30s
docker compose up -d --no-deps lb      # back to defaults
```

| | a) app caps only | b) lb: `maxconn` 200, queue 100 ms |
| --- | --- | --- |
| Accepted (`200`) p99 | 87.4 / 35.3 ms | 43.1 / 36.8 ms |
| Rejected (`503`) p99 | 277.9 ms / none shed | none shed / 132.7 ms |
| Share shed | 1.23% (3,686) / 0% | 0% / 0.14% (445) |
| Dropped | 949 / 0 | 0 / 0 |
| App CPU / lb CPU | 57% / 41% and 57% / 40% | 57% / 38% and 57% / 37% |
| Pass (spike SLO)? | ❌ (503 p99 278 ms, dropped 949) / ✅ | ✅ / ✅ |

**Question:** a `503` from the load balancer costs the app nothing. Does the accepted traffic get faster than in (a)? Where does the CPU go in each case?

**Recorded results (2026-11-10).** One instance, spike at 10,000 RPS for 30 s, two runs each (run 1 / run 2).

- **a) app caps only:** run 1: accepted p99 87.4 ms, `503` p99 277.9 ms (over the 200 ms limit), 1.23% shed (3,686), 949 dropped: FAIL. Run 2: accepted p99 35.3 ms, nothing shed, 0 dropped: PASS. The two runs disagree.
- **b) lb `maxconn` 200, queue 100 ms:** accepted p99 43.1 / 36.8 ms, shed 0% / 0.14% (445 `503`s from the lb queue timeout, p99 132.7 ms), 0 dropped, both PASS. The lb queued up to 901 / 1,399 requests at the peak (`qmax`).
- App CPU read 57% in all four runs (against a 50% quota; the sampling is per `docker stats`, not per scheduler period), lb 40-41% in (a) and 37-38% in (b).

**Caveat: the single instance was not overloaded.** At 10,000 RPS one instance shed at most 1.2% and used only 57% CPU, so this is not the 2x overload the step assumed. Level 3 measured about 5,000 RPS per instance, but in this setup (cache on, Redis, k6 at its own ceiling) a single instance almost keeps up with 10,000. Also, the 57% reading for one instance is much lower than three instances x 37% = 111% for the same 10,000 RPS in step 1; I don't know why, so don't read much into the CPU split.

**Answer to the question.** Not measurably: accepted p99 was 43 / 37 ms with the lb and 87 / 35 ms without, within the noise between runs. The lb kept the `503` p99 under the limit (133 ms) in the one run it shed, where the app-only run exceeded it (278 ms), but that is one run each. A `503` from the lb never reaches the app, so shedding there costs the app no CPU; in (a) the shed requests cost app CPU, in (b) the lb pays (queueing and `503`s), and the lb stayed at under 45% of its CPU. A real test needs an overload (more than 10,000 RPS, or a slowed instance with `make slow-app`), which I did not run.


## Step 5: 10,000 RPS and the first shared limit

```sh
make up
make load ENDPOINT='/products/{id}' RATE=10000 DURATION=60s
# while it runs, in another terminal:
docker stats --no-stream
docker compose exec redis redis-cli INFO stats | grep instantaneous_ops_per_sec
```

| Component | CPU at 8,000 RPS | CPU at 10,000 RPS | Near its limit? |
| --- | --- | --- | --- |
| app1 / app2 / app3 (limit 50% each) | 31.5 / 31.5 / 31.7% | 37.3 / 37.1 / 37.0% (run 1), 36.6 / 36.5 / 36.8% (run 2) | No (about 74% of the quota) |
| lb (limit 100%) | 38.5% | 47.0% / 44.2% | No |
| Redis (limit 50%, single-threaded) | 17.6% (about 9,200 ops/s) | 20.5% / 20.0% (about 11,350 ops/s) | No (about 40%) |
| Postgres (limit 100%) | 9.2% | 10.2% / 8.5% | No |
| k6 (limit 300%) | 96% | 112% / 106% (one 2 s sample at 338% during the stall in run 1) | Only in that stall sample |

| RATE | Achieved | p99 | Errors | Dropped | Pass? |
| --- | --- | --- | --- | --- | --- |
| 10000 | 9,775 / 9,999 | 110 ms / 3.37 ms | 0.05% (317) / 0% | 13,443 / 0 | ❌ (dropped 13,443) / ✅ |

**Question:** which component hits its limit first? **If k6 is the first, the honest result is "k6 capped, the system not yet": don't log 10,000 as passed.** What would you scale next?

**Recorded results (2026-11-10).** 10,000 RPS, 60 s, 3 instances, round-robin, two runs (run 1 / run 2). CPU is the steady-state average of samples every 2 s, as a percentage of one CPU; 8,000 RPS values are the run-1 averages from step 1 (run 2 within 1 point). Redis ops/s: about 9,200 at 8,000 RPS and about 11,350 at 10,000 RPS.

- **Run 1 failed**: 9,775 achieved, 13,443 dropped iterations, 317 failed requests (0.05%, all from app1, no DB timeouts or errors recorded), p99 110 ms. Around the 54th second one 2 s sample showed k6 at 338% (limit 300%) with Postgres and Redis at 31%, a stall; steady state before and after was normal (k6 about 110%). I did not find the cause of the stall or of app1's 5xx responses.
- **Run 2 passed everything**: 9,999 achieved, p99 3.37 ms, 0 errors, 0 dropped.
- In step 1 the 10,000 RPS runs were: 177 dropped (fail) and 0 dropped (pass). So four runs at 10,000: two pass, two fail the dropped-iterations check, with identical CPU profiles.

**Answer to the question.** In steady state no component hit its limit. The closest to a limit are the app instances at about 37% of a 50% quota (74%), then the lb at 47% of 100%, Redis at about 40% of its limit, k6 at 37% of its 300%, Postgres at 10%. The only component that went over its limit was k6 (338% in one sample) during run 1's stall. So the honest result is "10,000 RPS is at the edge of what k6 can drive on this Mac, and the system itself was not yet saturated"; I do **not** log 10,000 as passed, because only half the runs pass all three checks. Next to scale: the apps (add a fourth instance; they are the nearest to their quota), then Redis (single thread, grows with RPS) and the lb; also a better load generator (a second k6 or a separate host), since k6 sets the ceiling.


**Level 5 target:** `GET /products/{id}` passes the SLO at **10,000 RPS** through the load balancer.

---

## Pitfalls

- **Stale instances:** `make up` also removes the old single `app` container from Level 3 (it held port 8080).
- **`docker update` sticks:** `make slow-app` throttles until `make fast-app` or until the container is recreated.
- **A killed instance leaves the stats** as `MAINT (resolution)`, not `DOWN`. That's HAProxy saying Docker DNS no longer knows the name.
- **HAProxy threads:** `nbthread 2` is set in the config on purpose. HAProxy counts the Mac's cores, not the container's CPU quota; too many threads on a 1 CPU limit get throttled.
- **Always run each test twice.** The VM is at 7 of 8 CPUs, so noise is larger than before.
