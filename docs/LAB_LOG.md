# High-Concurrency HTTP Server: Lab Log

Last updated: 2026-11-06 · Current level: **3, step 2 done (timeouts bound a DB outage)** · Next: step 3 (N+1 vs join)

## 1. Goal and rules

Take a Go HTTP server from a real failure at about 100 RPS to 10,000 RPS, fixing one bottleneck per level, with graceful shutdown proven under load.

- **Fail first.** Every ceiling comes from a real cause (bugs, CPU quota, memory), never from an artificial rate limiter.
- **One change per level.** Fix one bottleneck, then re-measure, so every gain has a known cause.
- **I find the cause, Claude reviews.** Claude gives the symptom and a profiling hint; I diagnose and implement the fix. (Levels 1-2: Claude implemented on request and explained each step.)
- **Every result is logged** here with the exact command and numbers.

**SLO (pass/fail rule).** A level passes at a given RPS only if all three hold for the full test:

| Check | Threshold | Why |
| --- | --- | --- |
| p99 latency | < 200 ms | 99% of requests are fast |
| Error rate | < 0.1% | Failures, timeouts and resets count |
| Dropped iterations | 0 | Non-zero means the server was too slow and k6 ran out of virtual users. A handful at the start can be k6 adding VUs on demand, not the server (see F8) |

**Spike SLO (`make spike`, Level 2+).** Under overload a `503` is the right answer, so spike mode checks: accepted (`200`) p99 < 200 ms, rejected (`503`) p99 < 200 ms, anything else (timeouts, resets, other codes) < 0.1%, 0 dropped. Plus: the container never restarts and memory stays under 70% of the limit.

## 2. Lab environment

| Component | Value |
| --- | --- |
| Host | Apple M4, 10 cores (4 performance + 6 efficiency), 16 GB RAM |
| Docker | Docker Desktop, engine 28.0.4. VM: 8 CPUs / ~15.6 GB. **TODO: reduce to 6 CPUs / 8 GB** |
| App image | Go 1.25 on distroless, non-root. Go 1.25 sets GOMAXPROCS from the container CPU limit |
| App limits | Level 2 default: 0.5 CPU, 512 MB. Levels 0-1: 0.1 CPU, 128 MB. GOMAXPROCS = 2 at 0.5 CPU |
| Load generator | k6 1.0.0, own 3 CPU / 3 GB budget (raised from 2 / 1 GB in Level 2), runs inside the Docker network (targets `http://app:8080`). Tops out around 10,000 RPS on this Mac |
| Attack tool | `cmd/slowloris` (same image as the app), 0.5 CPU / 256 MB |
| Other containers | `db` and `sfa_api-localstack-1` from other projects share the Docker VM. Stop them for clean results |

**How to run a test**

```sh
make up                                       # build and start the app
make load ENDPOINT=/io RATE=100 DURATION=1m   # hold a fixed rate (pass/fail)
make ramp ENDPOINT=/io RATE=500               # climb 10 -> 500 RPS to find the ceiling
make spike ENDPOINT=/cpu RATE=4000 DURATION=30s  # overload test with the spike SLO
make slowloris CONNS=10000 DURATION=70s       # slow-connection attack (run beside a load test)
make stats                                    # live CPU and memory per container
APP_CPUS=0.1 APP_MEM=128m make up             # change app limits (default 0.5 / 512m)
MAX_INFLIGHT_IO=0 make up                     # turn load shedding off for an endpoint
curl -o cpu.pb.gz 'localhost:6060/debug/pprof/profile?seconds=10'   # pprof is on internal port 6060
```

**Lab limits.** Apple cores are faster than EC2 vCPUs, so treat ceilings as relative. T-series burst credits and real network latency are not emulated. Docker vCPUs can land on efficiency cores, so expect 10-20% run-to-run noise: run each test twice.

## 3. Current state: Level 2

Code: [cmd/server/main.go](../cmd/server/main.go) · Box: 0.5 CPU / 512 MB

| Endpoint | Simulates | Status |
| --- | --- | --- |
| `/fast` | Cheapest request (counter + JSON) | Atomic counter. In-flight cap 1,000 |
| `/io` | 10 ms downstream call (DB, API) | Ceiling ~8,000 RPS on 0.5 CPU. In-flight cap 1,000 |
| `/cpu` | CPU-heavy work (5,000 SHA-256 rounds, like password hashing) | ~1,400 accepted RPS on 0.5 CPU. In-flight cap = GOMAXPROCS (2) |
| `/healthz` | Liveness check | Never shed |
| `/debug/pprof/*` | Profiling | Internal port `:6060`, published on `127.0.0.1` only. `:8080/debug/pprof` returns 404 |

Server protections added in Level 2:

| Protection | Setting | Stops |
| --- | --- | --- |
| `ReadHeaderTimeout` / `ReadTimeout` | 5 s / 10 s | Slow clients holding connections (slowloris) |
| `WriteTimeout` / `IdleTimeout` | 15 s / 60 s | Stuck responses, idle keep-alive connections piling up |
| `MaxHeaderBytes` / `MaxBytesHandler` | 64 KB / 1 MB | Huge headers and bodies |
| Go memory limit | 90% of the cgroup limit, read at startup (460 MiB of 512). `GOMEMLIMIT` overrides | GC ignoring the container cap |
| Per-endpoint in-flight cap (`MAX_INFLIGHT_*`) | Full = immediate `503` + `Retry-After: 1` | Unbounded goroutines, memory and latency under spikes |

Still open: no graceful shutdown (SIGTERM drops in-flight requests), and block/mutex profiling is still on at rate 1 (some overhead).

## 4. Test results

### Level 3, step 2: query timeouts under a frozen database (2026-11-06)

`DB_POOL=on`, 300 RPS for 60 s, Postgres frozen for 10 s (`docker compose pause`). Full tables in [LEVEL3_GUIDE.md](LEVEL3_GUIDE.md#step-2-query-timeouts-a-stuck-database).

| | A: no timeout | B: `DB_TIMEOUT=300ms` |
| --- | --- | --- |
| During the freeze | requests hang (probe: 9.9 s) | `504` after 0.30 s |
| Errors / dropped | 8.6% (5 s timeouts) / **1,247** | 16.2% (`504`) / 35 |
| Peak goroutines / memory | **1,613 / ~40 MiB** | **210 / 12.8 MiB** |
| k6 users needed | 600 (its max) | 94 |
| Recovery after unfreeze | immediate | immediate |

### Level 3, step 1: connection pool (2026-11-06)

`DB_POOL=on`. Full tables in [LEVEL3_GUIDE.md](LEVEL3_GUIDE.md#step-1-connection-pool).

| Test | Result |
| --- | --- |
| 600 RPS (failed in step 0) | ✅ p99 3.1 ms, 0% errors, **10 connections in total**, Postgres CPU 100% → 13% |
| Pool size sweep at 1,500 RPS | All pass. Size 2: 20% of requests wait (p99 7.8 ms). **5-20 best** (p99 1.6-2.3 ms). 90: p99 20 ms, long waits (likely new connections opened during load) |
| Pool size sweep at 7,000 RPS | Inconclusive: all sizes failed by 0.6-1.9% in this batch, noise bigger than the effect |
| Ceiling (pool 20) | **~8,000 RPS** (8,000 ✅, 8,500 ❌ 0.21% shed). App CPU at its quota (52%), Postgres ~37% |
| Stability 5,500-7,000 | 5 of 13 runs failed by 0.1-2% `503`s, 0 DB errors (F11) |
| Warm pool (`DB_MIN_CONNS=10`, max 10) at 300 RPS | 0 waits vs 7 waits (~5 ms each, one per new connection) with a cold pool. Same p99 (1.5 ms): only matters when a pool grows under heavy load |

### Level 3, step 0: naive baseline (2026-11-06)

All switches off: a new Postgres connection per request, no timeouts, N+1 list, no cache. App 0.5 CPU / 512 MB, Postgres 1 CPU / 1 GB (`max_connections` 100), k6 `HOT_RATIO=0.8`. 30 s per run; the app restarts before each run so counters start at 0. CPU sampled every ~2 s.

| Endpoint | RATE | Achieved | p99 | Errors | Dropped | App CPU | Postgres CPU | Max PG conns | `db_connects` | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| `/products/{id}` | 100 | 100/s | 5.3 ms | 0% | 0 | 11-12% | 26-28% | 1 | 3,000 (1 per request) | PASS |
| `/products/{id}` | 300 (×2) | 300/s | 3.9 / 5.1 ms | 0% | 0 | 22% | 49-56% | 1 | 9,000 | PASS |
| `/products/{id}` | **340** | 340/s | 5.6 ms | 0% | 0 | 24-26% | 55-62% | 2 | 10,201 | **PASS (ceiling)** |
| `/products/{id}` | 375 | 285/s | 5 s | 36% | 1,486 | 27-33% | **100%** | 57 | 7,236 | FAIL |
| `/products/{id}` | 450 | 364/s | 5 s | 53% | 1,197 | 30-36% | 99-108% | 41 | 6,878 | FAIL |
| `/products/{id}` | 525 | 428/s | 5 s | 59% | 986 | 31-33% | 102-139% | 57 | 7,364 | FAIL |
| `/products/{id}` | 600 | 500/s | 5 s | 64% | 964 | 31-35% | 101-109% | 52 | 7,354 | FAIL |
| `/products?category={cat}` | 100 | 100/s | 6.3 ms | 0% | 0 | 14% | 29-31% | 1 | 3,001 (63,021 queries = 21 per request) | PASS |
| `POST /orders` | 100 | 100/s | 7.4 ms | 0% | 0 | 11-13% | 28-32% | 1 | 3,000 (6,000 queries) | PASS |

Errors in the failing runs: `db_errors` 1,408-1,690 per run (connection failures: `connection reset by peer` and `FATAL: sorry, too many clients already`). The rest were k6's 5 s timeouts. Postgres logged **7,275** "too many clients" rejections across the failing runs. `db_last_error` only shows the most recent error, so it doesn't show how often each kind happened.



**2a. Failures first (Level 1 code, 0.1 CPU / 128 MB)**

| Test | Result |
| --- | --- |
| `/io` spike 3,000 RPS, 30 s | Memory pinned at 127.8 of 128 MiB. 37% timeouts, 39,480 dropped, accepted p99 4.89 s. pprof stopped answering. No OOM kill, but useless |
| 1,000 slowloris connections + `/io` 300 RPS | Normal traffic passed (p99 116 ms), only 17 MiB used. But the server closed **0 of 1,000** slow connections |
| 10,000 slowloris connections + `/io` 300 RPS | **OOM-killed** (exit 137) and stayed down (no restart policy). Normal users: 84% failed |

**2b. Bigger box only (0.5 CPU / 512 MB, no protections)**

| Endpoint | RPS | p99 | Dropped | Result |
| --- | --- | --- | --- | --- |
| `/io` | 4,000 | 36 ms | 0 | PASS |
| `/io` | **8,000** | 71 ms | 0 | **PASS (ceiling, app CPU 55% = quota)** |
| `/io` | 10,000 | 126 ms | 4,929 (1.6%) | FAIL |
| `/cpu` | 700 | 38 ms | 0 | PASS (app CPU 51%) |
| `/cpu` spike 2,000 | accepted p99 3.94 s | 10,989 | FAIL: goroutines 4,000, memory 132 -> 209 MiB |

Runs at 6,000-10,000 before the k6 fix (F8) are not valid: k6 was at its memory and CPU limit while the app was at 16-33% CPU.

**2c-2d. Timeouts and memory limit**

| Test | Before | After |
| --- | --- | --- |
| 10,000 slowloris + 300 RPS, **0.1 CPU / 128 MB** | OOM-killed, normal users 84% failed | Up, peak 66 MiB, **10,000 of 10,000 closed by server**, 0% errors (p99 244 ms: quota throttling, F2) |
| 10,000 slowloris + 300 RPS, 0.5 CPU / 512 MB | - | Peak 68 MiB, all 10,000 closed, p99 13 ms |
| `/io` spike 3,000, 0.1 CPU / 128 MB + memory limit (115 MiB) | 127.8 MiB, 37% timeouts | 115-116 MiB, 25% timeouts. Headroom only; not a fix |

**2e. Load shedding (0.5 CPU / 512 MB)**

| Test | Cap | Accepted p99 | 503 p99 | Shed | Dropped | Peak memory | Result |
| --- | --- | --- | --- | --- | --- | --- | --- |
| `/cpu` spike 2,000 | none | 3.94 s | - | 0% | 10,989 | 209 MiB | FAIL |
| `/cpu` spike 2,000 | 50 | 3.44 s | 2.67 s | 0.8% | 10,794 | 106 MiB | FAIL: 50 CPU-bound requests at once still eat the whole quota |
| `/cpu` spike 2,000 | 2 | ~87 ms | ~168 ms | 30% | 0 | 19 MiB | PASS |
| `/io` spike 10,000 (k6 max, 1.25x ceiling) | 400 | 272 ms | 24 ms (p95) | 3% | 19,212 | 76 MiB | FAIL narrowly |
| `/io` spike 3,000 on 0.1 CPU / 128 MB | 400 | 4.2 s | 3.96 s | 13.5% | 30,594 | 109 MiB, no OOM | FAIL: box can't even afford to reject (F7) |
| `/io` normal 6,000 | 400 | 52 ms | - | 0.16% | 0 | - | FAIL (0.16% > 0.1%); raised cap to 1,000 |

**Final Level 2 tests (0.5 CPU / 512 MB, caps: `/io` 1,000, `/cpu` 2)**

| Test | Result | Numbers |
| --- | --- | --- |
| T1 normal `/io` 1,000 RPS, 1 min | **PASS** | p99 13 ms, 0 errors, 0 shed |
| T1b normal `/io` 6,000 RPS, 30 s, ×2 | **PASS** | p99 53 / 38 ms, 0 errors, 0 dropped |
| T2 spike `/cpu` 4,000 RPS (~3x capacity), 30 s | **PASS*** | accepted p99 145 ms, 503 p99 172 ms, 66% shed, 0 unexpected, peak 50 MiB. *5 of 120,000 dropped: k6 needed 801 VUs with 800 pre-allocated (F8) |
| T3 10,000 slowloris + `/io` 300 RPS | **PASS** | all 10,000 closed by server, normal p99 12 ms, 0 errors, peak 83 MiB, 2,350 goroutines |

### Levels 0-1

All runs: `make load` (constant arrival rate), SLO = p99 < 200 ms, errors < 0.1%, 0 dropped.

| Date | Level | Endpoint | Target RPS | Duration | Achieved RPS | p50 | p99 | Errors | Dropped | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2026-11-03 | 1 | `/io` | 2000 | 30s | 1,371 | 1.11 s | > 4.9 s (p95) | 4.78% | 13,900 | FAIL |
| 2026-11-03 | 1 | `/io` | 1500 | 30s | 1,437 | 186 ms | > 643 ms (p95) | 0% | 639 | FAIL |
| 2026-11-03 | 1 | `/io` | 1250 | 30s | 1,227 | - | 653 ms | 0% | 107 | FAIL |
| 2026-11-03 | 1 | `/io` | 1000 | 30s | 994 | - | 526 ms | 0% | 68 | FAIL (an earlier run had 0 dropped; it's borderline) |
| 2026-11-03 | 1 | `/io` | **875** | 30s | 872 | - | 191 ms | 0% | 0 | **PASS (ceiling)** |
| 2026-11-03 | 1 | `/io` | 750 | 30s ×2 | 749 | - | 165 / 178 ms | 0% | 0 | PASS |
| 2026-11-03 | 1 | `/io` | 500 | 1m | 500 | 26 ms | 124 ms | 0% | 0 | PASS (Level 1 goal) |
| 2026-11-03 | 0 | `/io` | 150 | 40s | 89 | 3.33 s | 3.42 s | 0% | 2,142 | FAIL (re-run before the fix) |
| 2026-10-31 | 0 | `/io` | 150 | 20s | **~89 (ceiling)** | 3.24 s | 3.43 s | 0% | 917 | FAIL |
| 2026-10-31 | 0 | `/io` | 80 | 20s | 80 | 12 ms | 111 ms | 0% | 0 | PASS |
| _pending_ | - | `/fast` | - | | | | | | | _not measured yet_ |

**Profile at 1000 RPS after the fix (10 s captures):**

| Signal | Value | Meaning |
| --- | --- | --- |
| App CPU (`docker stats`) | 10.7% | Pinned at the 0.1 CPU quota |
| CPU profile | 900 ms of CPU in 10 s (9%) | Same: the quota is used up |
| App memory | 96 MiB of 128 MiB (75%) | Getting close to the cap, with no GOMEMLIMIT |
| Block profile | `sync.(*Cond).Wait`, `runtime.selectgo` only | Normal net/http idle waits and pprof's own timer. `ioHandler` and `Mutex.Lock` are gone |
| Goroutines | 513 | Up to ~250 in-flight requests, each with a connection-reader goroutine |

## 5. Failures and diagnosis

**F1. `/io` capped at ~89 RPS: RESOLVED in Level 1**
- Cause: the global mutex was held during the 10 ms simulated call, so requests ran one at a time. Theoretical max = 1 s / 10 ms = 100 RPS; CPU throttling brought it down to ~89.
- Fix: replaced the mutex and int with `atomic.Int64`. Each handler reads the counter once with `requests.Add(1)` and uses that local value in the reply.
- Result: ceiling went from ~89 to ~875 RPS (about 10×) on the same 0.1 CPU box.
- Profile evidence before the fix (`/io` at 150 RPS, 20 s captures):

  | Profile | Total delay in 20 s | Top frame (flat) | Caller (cum) |
  | --- | --- | --- | --- |
  | block | 1.66 h (≈300 goroutines waiting at once) | `sync.(*Mutex).Lock` 99.67% | `main.ioHandler` |
  | mutex | 1.28 h | `sync.(*Mutex).Unlock` 100% | `main.ioHandler` |

- Check-yourself answers:
  1. 1.66 h = 5,976 s ÷ 20 s ≈ **299 goroutines waiting at once**. It comes from k6's `maxVUs = RATE × 2 = 300`: every virtual user was stuck. A live goroutine dump showed 603 in total: 299 in `Mutex.Lock`, 1 in `time.Sleep`, and 301 net/http connection readers (one per open connection, idle and harmless).
  2. They waited for the global mutex `mu` at `ioHandler` line 58 (`mu.Lock()`). They were waiting on **one** goroutine that held the lock while sleeping in `time.Sleep` (line 62). The mutex profile charges the delay to that holder's `Unlock`.
  3. `defer mu.Unlock()` runs when the function returns, so `requests++`, `time.Sleep` **and** `writeJSON` all ran under the lock. Only `requests++` and the read of `requests` touch shared state; the sleep and the reply don't need the lock.

  | Line | Held under lock? | Touches shared state? | Needs the lock? |
  | --- | --- | --- | --- |
  | `requests++` | Yes | Yes | Yes (or an atomic) |
  | `time.Sleep(...)` | Yes | No | No |
  | `writeJSON(... requests)` | Yes | Yes (reads `requests`) | No, if the value is saved earlier |

**F2. Tail latency from CPU throttling: OPEN (smaller at 0.5 CPU)**
- Symptom: p99 is much higher than p50 even in passing runs (Level 1 at 500 RPS: p50 26 ms, p99 124 ms).
- Cause: a 0.1 CPU limit lets the app use 10 ms of CPU in every 100 ms window. Once used up, the process waits for the next window, adding up to ~100 ms to unlucky requests.
- Not a code bug; it shows the cost of an undersized instance.
- Level 2: still visible. On 0.5 CPU, `503`s take up to ~170 ms at p99 because the kernel pauses the whole process when the quota runs out. That's why the spike SLO allows 200 ms for `503`s, not 20 ms.

**F3. `/io` ceiling ~875 RPS from the CPU quota: RESOLVED by the bigger box (Level 2)**
- Symptom: at 1000+ RPS, p99 goes above 500 ms and k6 drops requests; at 2000 RPS, 4.8% of requests fail.
- Cause: the app uses its full 0.1 CPU (10.7% in `docker stats`, 9% in the CPU profile). The code no longer serializes requests; the box is simply too small.
- Level 2: on 0.5 CPU the `/io` ceiling is ~8,000 RPS (about 9x, for 5x the CPU: less throttling overhead).

**F4. Memory at 75% of the cap with no protection: RESOLVED in Level 2 (memory limit + load shedding)**
- Symptom: 96 MiB of 128 MiB used at 1000 RPS.
- Risk: under a bigger spike, more in-flight requests means more goroutines and buffers. Without `GOMEMLIMIT` or an in-flight limit, the container can be OOM-killed and drop every request at once.
- **Investigation (2026-11-03, `/io` spike at 2000 RPS for 30 s, box 0.1 CPU / 128 MB):**

  | Moment | THP on (default) | THP off (`GODEBUG=disablethp=1`) |
  | --- | --- | --- |
  | Fresh start | 3 MiB | 2 MiB |
  | During spike | 107-123 MiB, 2,900-3,700 goroutines, live heap 48-65 MiB | 87 MiB, 2,525 goroutines, live heap 38 MiB |
  | 10 s after | 107-113 MiB (76 MiB of it in huge pages) | 79 MiB |
  | 150 s after | 96 MiB | **33 MiB** |

  - **Why it rises:** memory follows the number of **in-flight** requests, not RPS. Little's Law: in-flight = rate × latency. Overloaded, latency grows, so in-flight grows to thousands. Each one holds a goroutine stack, connection buffers and request objects. At 123 MiB the box was 5 MiB from an OOM kill.
  - **Why it stays high:** Go keeps freed memory for reuse and returns it to the OS slowly. When idle, it only forces a GC every 2 minutes. On top of that, Docker Desktop's kernel (6.10 linuxkit) has transparent huge pages (THP) set to `always`, with `khugepaged/max_ptes_none=511`. The kernel re-packs memory Go has released into 2 MB huge pages, so Docker still counts it. One idle snapshot showed Go's live heap at 3.7 MiB, Go reporting 96 MiB released, and 88 of the process's 92 MiB in huge pages.
  - **Caveat:** the THP-off run had 22.6% errors and ran longer than planned, so its "during" numbers are less comparable. The "150 s after" difference (96 vs 33 MiB) is the clean result.
  - **Fix plan (Level 2):** cap in-flight requests (load shedding) and set `GOMEMLIMIT`. `GODEBUG=disablethp=1` is optional; the real fix is not letting in-flight requests grow without limit.
- **Result:** `/cpu` spike memory went from 209 MiB to 19-50 MiB; goroutines from 4,000 to ~405.

**F5. Slow clients hold connections forever (slowloris): RESOLVED in Level 2**
- Symptom: the server closed 0 of 10,000 slow connections; at 128 MB it was OOM-killed.
- Fix: `ReadHeaderTimeout` 5 s (plus read/write/idle timeouts). All 10,000 now get cut off.

**F6. A concurrency cap of 50 didn't help CPU-bound `/cpu`: RESOLVED in Level 2**
- Symptom: with cap 50, only 0.8% shed, but `503`s took 2.7 s.
- Diagnosis: a goroutine dump showed 2,616 goroutines runnable in `readRequest`, waiting for CPU before they could reach the limiter. A CPU profile showed 92% of CPU in `cpuHandler`. With 50 CPU-bound requests running, each got 1/50 of the CPU and the network side starved.
- Fix: cap CPU-bound work at GOMAXPROCS (2). More in flight than cores adds only queueing. Little's Law sizing works for I/O-bound work, not CPU-bound.
- Also found: the earlier `/cpu` "ceiling" of 700 RPS was only the highest rate tested. With the cap, it serves ~1,400 accepted RPS.

**F7. In-process shedding can't save a box that can't afford to say no: OPEN (Level 5)**
- Symptom: `/io` spike at 3,000 RPS on 0.1 CPU still has 27% timeouts with the cap; at 10,000 RPS on 0.5 CPU, 6% dropped.
- Cause: accepting, parsing and answering a request costs CPU **before** the limiter runs. When arrivals × that cost exceeds the CPU, even `503`s queue up.
- Fix: shed in front of the server (load balancer / edge rate limit, Level 5) or add capacity.

**F8. k6 itself was the bottleneck above 5,000 RPS: RESOLVED (tooling)**
- Symptom: k6 at 2 GiB / 2 CPU limits while the app used 16-33% CPU.
- Cause: k6 pre-allocated `RATE x 2 / 4` VUs (thousands).
- Fix: pre-allocate `RATE / 5` VUs (enough for 200 ms latency), grow on demand; k6 budget 3 CPU / 3 GB. A few dropped iterations at the start can still come from VU growth.

**F9. A new connection per request makes Postgres CPU the ceiling (~340 RPS): RESOLVED in Level 3, step 1 (pool: ~8,000 RPS)**
- Symptom: `/products/{id}` passes at 340 RPS, then collapses at 375: 36% errors, p99 5 s, Postgres CPU at 100% of its 1 CPU. The app sits at ~30% CPU, so it isn't the bottleneck.
- Cause: every request opens a new connection. Postgres forks a new backend process and checks the password (SCRAM) each time. That costs Postgres ~1.7 ms of CPU per request (50% CPU at 300 RPS), and almost all of it is connection setup, not the query: the list endpoint runs **21 queries** per request and costs about the same Postgres CPU as the 1-query product endpoint (29% vs 27% at 100 RPS).
- **It's a cliff, not a slope:** Postgres CPU was 57% at 340 RPS but 100% at 375. Once Postgres falls slightly behind, connections take longer, so more are open at once (1-2 jumps to 41-57). Each one is another process competing for the same CPU, so everything slows further: a feedback loop. Near the limit of 100 connections, Postgres starts refusing (`too many clients`) and resetting connections.
- Fix (step 1): a connection pool. Connections are opened once and reused, so the setup cost disappears and the number of connections is capped.

**F10. Work continues for clients that already gave up: RESOLVED in Level 3, step 2 (`DB_TIMEOUT`)**
- Symptom: in the failing runs, the app completed more queries than successful responses. At 375 RPS, 7,236 queries ran but only 6,225 requests succeeded, so ~1,000 queries finished after k6 had already timed out at 5 s. At 600 RPS: 7,354 vs 6,129.
- Cause: the naive code uses `context.Background()`, so DB work ignores the client hanging up and has no deadline. During overload, the database spends CPU on answers nobody receives.
- Fix (step 2): `DB_TIMEOUT` ties DB work to the request's context and adds a deadline.
- Result: during a 10 s database freeze, failed requests end after 0.30 s with `504`; goroutines stay at ~210 instead of 1,613, memory at 12.8 instead of ~40 MiB.

**F11. Brief stalls near the ceiling make the in-flight cap shed: OPEN (investigate)**
- Symptom: with the pool on, 5 of 13 runs between 5,500 and 7,000 RPS failed by 0.1-2% `503`s, while repeats at the same or higher rates passed (up to 8,000). 0 database errors.
- What's known: the `503`s come from the in-flight cap (1,000), so in-flight requests briefly piled up. At 6,000 RPS the app is only at 42% CPU, so it isn't plain CPU saturation.
- Suspects (not verified): CPU-quota pauses (F2), GC pauses, or activity on the Mac. Next: capture a `go tool trace` or GC stats during a failing run.

**F12. k6 created one time series per unique URL: RESOLVED (tooling)**
- k6 warned of 800,000+ unique time series: each request's URL is unique (random id + `r=` suffix), so k6's memory grew without bound. Requests are now tagged with the endpoint template (`tags: { name: ENDPOINT }`).

## 6. Level 3 plan: Postgres + Redis

**Decided 2026-11-06:** add a real Postgres and Redis. Claude implements the code; I run the tests myself with [LEVEL3_GUIDE.md](LEVEL3_GUIDE.md).

- **New:** Postgres 16 (1 CPU / 1 GB, 100k products), Redis 7 (0.5 CPU / 256 MB). Endpoints `GET /products/{id}`, `GET /products?category=`, `POST /orders`.
- **Every fix is a switch, off by default (naive):** `DB_POOL`, `DB_TIMEOUT`, `LIST_QUERY=join`, `CACHE`, `SINGLEFLIGHT`.
- **Steps:** 0 naive baseline → 1 connection pool (and pool sizing) → 2 query timeouts under a frozen DB → 3 N+1 vs join → 4 Redis cache-aside (hit ratio vs traffic skew) → 5 cache stampede with singleflight. Bonus: hot-row lock contention on orders.
- **Target:** `GET /products/{id}` passes the SLO at 5,000 RPS on the 0.5 CPU app.
- **Claude's smoke checks (single requests, not load tests):** each switch does what it says. Pool: 20 requests used 1 connection. Timeout: frozen DB gives `504` in 0.30 s vs a 3.2 s hang. List: 21 queries vs 1. Cache: an order invalidates the cached product. Singleflight: 300 parallel misses on one key make 300 queries vs 1.

## 7. Level roadmap

| Level | Box (CPU / RAM) | Target | Expected bottleneck | Fix to learn |
| --- | --- | --- | --- | --- |
| 0 | 0.1 / 128 MB | ~100 RPS (fails) | Global lock, CPU throttling | Baseline, profiling with pprof |
| 1 | 0.1 / 128 MB | 500 (reached ~875) | Lock held during I/O | Narrow critical sections, atomics |
| 2 | 0.5 / 512 MB | 1,000 (reached: `/io` ceiling ~8,000; survives 3x `/cpu` spike and 10k slowloris) | Unbounded goroutines and memory under spikes | Timeouts, size limits, GOMEMLIMIT, bounded concurrency, load shedding (fast 503) |
| 3 | app 0.5 / 512 MB + Postgres 1 / 1 GB + Redis 0.5 / 256 MB | `GET /products/{id}` 5,000 | Connection churn, no timeouts, N+1, no cache, stampedes | Pooling, timeouts, joins, cache-aside, singleflight |
| 4 | 0.5 / 512 MB | CPU per request down | GC pressure from allocations | Allocation profiling, sync.Pool, cheaper JSON |
| 5 | 3 app instances + load balancer | 10,000 | A single instance's limit; F7 | Horizontal scaling, load balancer, shedding at the edge |
| 6 | same as 5 | 10,000, 0 errors during redeploys | Dropped requests on shutdown | Graceful shutdown, readiness draining |

`/cpu` at 10k RPS would need ~3.5 full cores (measured ~1,400 RPS per 0.5 CPU). It will hit the hardware limit first, so its target is set separately. Roadmap re-baselined 2026-11-06.

## 8. Change log

Newest first. Each entry: what changed, before and after numbers, what limits it next.

| Date | Level | Change | Before | After | Next bottleneck |
| --- | --- | --- | --- | --- | --- |
| 2026-11-06 | 3 (step 2) | Measured `DB_TIMEOUT=300ms` vs none during a 10 s DB freeze (no code change). | Freeze: requests hang up to 10 s, 1,613 goroutines, 1,247 dropped | `504` in 0.30 s, 210 goroutines, 35 dropped | N+1 list query (step 3) |
| 2026-11-06 | 3 (step 1) | Connection pool on (`DB_POOL=on`). k6: tag requests by endpoint template (F12). | `/products/{id}` ceiling ~340 RPS | ~8,000 RPS (app CPU at quota) | Intermittent shedding near the ceiling (F11); no deadlines (F10) |
| 2026-11-06 | 3 (step 0) | Postgres + Redis added; all fixes behind switches, off by default. Measured the naive baseline. | - | `/products/{id}` ceiling ~340 RPS (375 collapses, Postgres CPU 100%) | Connection per request (F9), no deadlines (F10) |
| 2026-11-05 | 2 | Load shedding: per-endpoint in-flight caps with immediate `503` + `Retry-After`. `/io` and `/fast` 1,000; `/cpu` = GOMAXPROCS (a cap of 50 failed, F6). pprof moved to internal `:6060`. | `/cpu` 2,000 spike: p99 3.94 s, 10,989 dropped, 209 MiB | `/cpu` 4,000 spike: accepted p99 145 ms, 66% shed, 50 MiB | F7: shedding in front of the server |
| 2026-11-05 | 2 | Timeouts, header/body limits, Go memory limit from cgroup (90%). | 10k slowloris: OOM-killed | All 10k closed by server, 0% errors | Unbounded in-flight requests |
| 2026-11-05 | 2 | Bigger box: 0.5 CPU / 512 MB. Tooling: `make spike` (spike SLO), `cmd/slowloris`, k6 VU pre-allocation fix and 3 CPU / 3 GB budget. | `/io` ceiling ~875 RPS (0.1 CPU) | `/io` ceiling ~8,000 RPS | Overload protection |
| 2026-11-03 | 1 | Fixed F1: replaced the global mutex with `atomic.Int64`; each handler reads the counter once and never holds anything during I/O. | `/io` ceiling ~89 RPS | `/io` ceiling ~875 RPS (1000 fails) | CPU quota (F3), memory headroom (F4) |
| 2026-11-03 | 1 | Added pprof profiling (`net/http/pprof`, block and mutex rates = 1). Profiles confirm F1. Pitfalls hit: stale container served 404 (rebuild with `make up`), Graphviz missing, port 9090 in use. | - | - | - |
| 2026-10-31 | 0 | Lab created: naive server, EC2-sized compose limits, k6 open-model test, Makefile. Worked around a Compose v2 panic on `run -e` (k6 service needs an `environment:` block). | - | `/io` ceiling ~89 RPS | Global lock on `/io` (F1) |
