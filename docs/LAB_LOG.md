# High-Concurrency HTTP Server: Lab Log

Last updated: 2026-10-31 · Current level: **0 (baseline, failing)**

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

## 3. Current state: Level 0

Code: [cmd/server/main.go](../cmd/server/main.go)

| Endpoint | Simulates | Deliberate problem |
| --- | --- | --- |
| `/fast` | Cheapest request (counter + JSON) | Shares the global lock with `/io` |
| `/io` | 10 ms downstream call (DB, API) | Holds the global lock while waiting |
| `/cpu` | CPU-heavy work (5,000 SHA-256 rounds, like password hashing) | None in code; limited by CPU quota |
| `/healthz` | Liveness check | - |

Server-wide problems:

- No timeouts (`ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout`): open to slowloris.
- No request body or header size limits.
- No `GOMEMLIMIT`: the GC doesn't know about the 128 MB cap.
- No graceful shutdown: SIGTERM kills the process and drops in-flight requests.

## 4. Test results: Level 0

| Date | Endpoint | Mode | Target RPS | Achieved RPS | p50 | p99 | Errors | Dropped | Result |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 2026-10-31 | `/io` | constant 20s | 80 | 80 | 12 ms | 111 ms | 0% | 0 | PASS |
| 2026-10-31 | `/io` | constant 20s | 150 | **~89 (ceiling)** | 3.24 s | 3.43 s | 0% | 917 | FAIL |
| _pending_ | `/fast` | ramp | 3000 | | | | | | _you: predict, then measure_ |
| _pending_ | `/cpu` | ramp | 300 | | | | | | _you: predict, then measure_ |

## 5. Current failures and diagnosis

**F1. `/io` caps at ~89 RPS (ceiling)**
- Symptom: above ~89 RPS, requests queue; p99 jumps from 111 ms to 3.4 s and k6 drops requests.
- Cause: the global mutex is held during the 10 ms simulated call, so requests run one at a time. Theoretical max = 1 s / 10 ms = 100 RPS; CPU throttling brings it down to ~89.
- Side effect to verify: `/fast` should also slow down under mixed traffic, because it waits for the same lock.

**F2. p99 of 111 ms even when passing (tail latency)**
- Symptom: at 80 RPS the median is 12 ms but p99 is 111 ms.
- Cause: a 0.1 CPU limit lets the app use 10 ms of CPU in every 100 ms window. Once used up, the process waits for the next window, adding ~100 ms to unlucky requests.
- Not a code bug; it shows the cost of an undersized instance. Fixed by giving more CPU in a later level.

## 6. Fix plan: Level 1

**Goal:** `/io` passes the SLO at 500 RPS on the same 0.1 CPU / 128 MB box.

**Your task (F1):**
1. Prove the cause with a profile before changing code. Hint: Go's `net/http/pprof` has a **mutex** profile and a **block** profile. Look at `runtime.SetMutexProfileFraction` and `runtime.SetBlockProfileRate`.
2. Fix the code so a request never holds a lock while waiting on I/O. Ask yourself what the lock actually needs to protect.
3. Bonus: is a mutex the best tool for a single counter? (Look at `sync/atomic`.)

**Verify:**
```sh
make up
make load ENDPOINT=/io RATE=500 DURATION=1m   # must PASS
make ramp ENDPOINT=/io RATE=3000              # record the new ceiling
```

**Expected:** with the lock gone, `/io` is limited by CPU and memory per in-flight request, not by serialization. Record the new ceiling and what limits it next.

## 7. Level roadmap

| Level | Box (CPU / RAM) | Target | Expected bottleneck | Fix to learn |
| --- | --- | --- | --- | --- |
| 0 | 0.1 / 128 MB | ~100 RPS (fails) | Global lock, CPU throttling | Baseline, profiling with pprof |
| 1 | 0.1 / 128 MB | 500 | Lock held during I/O | Narrow critical sections, atomics |
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
| 2026-10-31 | 0 | Lab created: naive server, EC2-sized compose limits, k6 open-model test, Makefile. Worked around a Compose v2 panic on `run -e` (k6 service needs an `environment:` block). | - | `/io` ceiling ~89 RPS | Global lock on `/io` (F1) |
