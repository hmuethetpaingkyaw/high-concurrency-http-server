# High-Concurrency HTTP Server: Lab Log

Last updated: 2026-11-03 · Current level: **1 (done, ceiling ~875 RPS)** · Next: Level 2

## 1. Goal and rules

Take a Go HTTP server from a real failure at about 100 RPS to 10,000 RPS, fixing one bottleneck per level, with graceful shutdown proven under load.

- **Fail first.** Every ceiling comes from a real cause (bugs, CPU quota, memory), never from an artificial rate limiter.
- **One change per level.** Fix one bottleneck, then re-measure, so every gain has a known cause.
- **I find the cause, Claude reviews.** Claude gives the symptom and a profiling hint; I diagnose and implement the fix.
- **Every result is logged** here with the exact command and numbers.

**SLO (pass/fail rule).** A level passes at a given RPS only if all three hold for the full test:

| Check | Threshold | Why |
| --- | --- | --- |
| p99 latency | < 200 ms | 99% of requests are fast |
| Error rate | < 0.1% | Failures, timeouts and resets count |
| Dropped iterations | 0 | Non-zero means the server was too slow and k6 ran out of virtual users |

## 2. Lab environment

| Component | Value |
| --- | --- |
| Host | Apple M4, 10 cores (4 performance + 6 efficiency), 16 GB RAM |
| Docker | Docker Desktop, engine 28.0.4. VM: 8 CPUs / ~15.6 GB. **TODO: reduce to 6 CPUs / 8 GB** |
| App image | Go 1.25 on distroless, non-root. Go 1.25 sets GOMAXPROCS from the container CPU limit |
| App limits (Level 0) | 0.1 CPU, 128 MB (a starved t3.nano) |
| Load generator | k6 1.0.0, own 2-CPU budget, runs inside the Docker network (targets `http://app:8080`) |

**How to run a test**

```sh
make up                                       # build and start the app
make load ENDPOINT=/io RATE=100 DURATION=1m   # hold a fixed rate (pass/fail)
make ramp ENDPOINT=/io RATE=500               # climb 10 -> 500 RPS to find the ceiling
make stats                                    # live CPU and memory per container
APP_CPUS=0.5 APP_MEM=512m make up             # change app limits for a level
```

**Lab limits.** Apple cores are faster than EC2 vCPUs, so treat ceilings as relative. T-series burst credits and real network latency are not emulated. Docker vCPUs can land on efficiency cores, so expect 10-20% run-to-run noise: run each test twice.

## 3. Current state: Level 1

Code: [cmd/server/main.go](../cmd/server/main.go) · Box: 0.1 CPU / 128 MB (unchanged from Level 0)

| Endpoint | Simulates | Status |
| --- | --- | --- |
| `/fast` | Cheapest request (counter + JSON) | Fixed in Level 1: atomic counter, no lock |
| `/io` | 10 ms downstream call (DB, API) | Fixed in Level 1: no lock held while waiting. Now limited by the CPU quota |
| `/cpu` | CPU-heavy work (5,000 SHA-256 rounds, like password hashing) | No code problem; limited by the CPU quota |
| `/healthz` | Liveness check | - |
| `/debug/pprof/*` | Profiling (added in Level 1) | Lab-only: exposed on the public port; move to an internal port in Level 2 |

Server-wide problems still open:

- No timeouts (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`): open to slowloris.
- No request body or header size limits.
- No `GOMEMLIMIT`: the GC doesn't know about the 128 MB cap.
- No limit on in-flight requests: a traffic spike creates unbounded goroutines.
- No graceful shutdown: SIGTERM kills the process and drops in-flight requests.

## 4. Test results

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
| _pending_ | - | `/fast` | ramp to 3000 | | | | | | | _not measured yet_ |
| _pending_ | - | `/cpu` | ramp to 300 | | | | | | | _not measured yet_ |

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

**F2. Tail latency from CPU throttling: OPEN**
- Symptom: p99 is much higher than p50 even in passing runs (Level 1 at 500 RPS: p50 26 ms, p99 124 ms).
- Cause: a 0.1 CPU limit lets the app use 10 ms of CPU in every 100 ms window. Once used up, the process waits for the next window, adding up to ~100 ms to unlucky requests.
- Not a code bug; it shows the cost of an undersized instance.

**F3. `/io` ceiling ~875 RPS from the CPU quota: OPEN (next bottleneck)**
- Symptom: at 1000+ RPS, p99 goes above 500 ms and k6 drops requests; at 2000 RPS, 4.8% of requests fail.
- Cause: the app uses its full 0.1 CPU (10.7% in `docker stats`, 9% in the CPU profile). The code no longer serializes requests; the box is simply too small.

**F4. Memory at 75% of the cap with no protection: OPEN (risk)**
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

## 6. Fix plan: Level 2

**Goal:** a bigger box (0.5 CPU / 512 MB) passes the SLO at 1,000 RPS, and a traffic spike far above the ceiling gets fast 503s instead of a crash or timeouts.

**Steps (one change at a time, measure after each):**
1. **Show the failure first.** On the current box, send a spike well above the ceiling (e.g. `make load ENDPOINT=/io RATE=3000 DURATION=30s`) and watch `make stats`. Record memory, errors, and whether the container restarts.
2. **Bigger box only.** `APP_CPUS=0.5 APP_MEM=512m make up`, then find the new ceiling. This isolates how much the CPU quota alone was costing (F2, F3).
3. **Server protections:** add read/write/idle timeouts, header and body size limits, and set `GOMEMLIMIT` to about 90% of the memory limit.
4. **Load shedding:** cap in-flight requests and return a fast `503` when full. Re-run the spike from step 1 and compare.
5. **Move pprof** to a separate internal port (e.g. `:6060`) that isn't published.

**Verify:**
```sh
APP_CPUS=0.5 APP_MEM=512m make up
make load ENDPOINT=/io RATE=1000 DURATION=1m   # must PASS
make load ENDPOINT=/io RATE=3000 DURATION=30s  # spike: fast 503s, no crash, no OOM
```

## 7. Level roadmap

| Level | Box (CPU / RAM) | Target | Expected bottleneck | Fix to learn |
| --- | --- | --- | --- | --- |
| 0 | 0.1 / 128 MB | ~100 RPS (fails) | Global lock, CPU throttling | Baseline, profiling with pprof |
| 1 | 0.1 / 128 MB | 500 (reached ~875) | Lock held during I/O | Narrow critical sections, atomics |
| 2 | 0.5 / 512 MB | 1,000 | Unbounded goroutines and memory under spikes | Timeouts, size limits, GOMEMLIMIT, bounded concurrency, load shedding (fast 503) |
| 3 | 0.5 / 512 MB | 2,500 | Slow dependency calls | Caching, connection reuse (DB/Redis added here) |
| 4 | 1 / 1 GB | 5,000 | GC pressure from allocations | Allocation profiling, sync.Pool, cheaper JSON |
| 5 | 3 × (1 / 1 GB) + nginx | 8,000 | A single instance's limit | Horizontal scaling, load balancer, upstream keep-alive |
| 6 | 3 × (1 / 1 GB) + nginx | 10,000, 0 errors during redeploys | Dropped requests on shutdown | Graceful shutdown, readiness draining |

`/cpu` at 10k RPS would need ~10 full cores. It will hit the hardware limit first, so its target is set separately once measured.

## 8. Change log

Newest first. Each entry: what changed, before and after numbers, what limits it next.

| Date | Level | Change | Before | After | Next bottleneck |
| --- | --- | --- | --- | --- | --- |
| 2026-11-03 | 1 | Fixed F1: replaced the global mutex with `atomic.Int64`; each handler reads the counter once and never holds anything during I/O. | `/io` ceiling ~89 RPS | `/io` ceiling ~875 RPS (1000 fails) | CPU quota (F3), memory headroom (F4) |
| 2026-11-03 | 1 | Added pprof profiling (`net/http/pprof`, block and mutex rates = 1). Profiles confirm F1. Pitfalls hit: stale container served 404 (rebuild with `make up`), Graphviz missing, port 9090 in use. | - | - | - |
| 2026-10-31 | 0 | Lab created: naive server, EC2-sized compose limits, k6 open-model test, Makefile. Worked around a Compose v2 panic on `run -e` (k6 service needs an `environment:` block). | - | `/io` ceiling ~89 RPS | Global lock on `/io` (F1) |
