# Concepts: Profiles, Ceilings, and Ramps

This page explains three ideas from Level 0, using your own numbers:

1. What profiles are, how to read them, and how to monitor.
2. What the "ceiling" is.
3. What `make ramp` is for.

---

## 1. Profiles

### 1.1 What is a profile?

A profile is a record of **where the program spends time or memory**.
Go records it while the program runs.

`pprof` is **on demand**. You ask: "record for 20 seconds". It records, then sends you the result.
It is a snapshot, not a live monitor. If no load is running during those 20 seconds, the profile is empty.

```
you ──"give me 20s of block data"──> server (/debug/pprof/block?seconds=20)
                                        │  records 20s
you <──────────── profile file ─────────┘
```

### 1.2 Block profile vs mutex profile

| Profile | Question it answers | Where it puts the time |
| --- | --- | --- |
| **block** | "Who **waited**, and where?" | At the wait: `Mutex.Lock`, channel send/receive, `select`, `WaitGroup.Wait` |
| **mutex** | "Who **made others wait**?" | At `Mutex.Unlock`: the goroutine that **held** the lock |

Note: the block profile does **not** record `time.Sleep`. Sleeping is not "blocked on another goroutine".
So the huge number you see is not the 10 ms sleep itself. It is goroutines waiting for the lock.

Your two profiles agree:

- block: goroutines waited in `Mutex.Lock`, called from `main.ioHandler`.
- mutex: the goroutine that held the lock released it in `Unlock`, also inside `main.ioHandler`.

Same handler on both sides: `ioHandler` waits for the lock **and** holds it.

### 1.3 How to read `-top`

Your block profile:

```
      flat  flat%   sum%        cum   cum%
   1.66hrs 99.67% 99.67%    1.66hrs 99.67%  sync.(*Mutex).Lock (inline)
         0     0% 99.67%    1.66hrs 99.67%  main.ioHandler
         0     0% 99.67%    1.66hrs   100%  net/http.(*ServeMux).ServeHTTP
         0     0% 99.67%    1.66hrs   100%  net/http.(*conn).serve
```

- **flat** = time spent **in this function itself**.
- **cum** (cumulative) = time in this function **plus everything it calls**.
- **sum%** = running total of `flat%` down the list.

Read the call chain from the bottom up:

```
net/http.(*conn).serve          flat 0      cum 1.66h
  └─ ServeMux.ServeHTTP         flat 0      cum 1.66h
       └─ main.ioHandler        flat 0      cum 1.66h
            └─ Mutex.Lock       flat 1.66h  cum 1.66h   <-- the actual waiting
```

`ioHandler` has `flat 0` because it does not wait by itself. It **calls** `Lock`, and `Lock` waits.
So `flat` shows **where** the waiting happens. `cum` shows **who called it**.

Rule of thumb: sort by `flat` to find the hot spot. Look at `cum` to find **your** code above it.

### 1.4 The key insight: "1.66 hours in 20 seconds"

```
Duration: 20.01s, Total samples = 1.66hrs (29944.06%)
```

How can 20 seconds contain 1.66 hours of waiting?
Because the profile **adds up the waiting of every goroutine**.

```
1.66 h  =  ~5,990 s of waiting (all goroutines added together)
5,990 s ÷ 20 s  =  ~300 goroutines waiting at every moment
```

The `29944%` says the same thing: 299 × 100% of the 20 s window.

```
time ->   0s ............................................. 20s
goroutine 1    [waiting for lock .............................]
goroutine 2    [waiting for lock .............................]
goroutine 3    [waiting for lock .............................]
   ...
goroutine 300  [waiting for lock .............................]

total = 300 × 20 s = 6,000 s ≈ 1.66 h
```

Why 300? k6 had `maxVUs = 300` (RATE 150 × 2).
All 300 virtual users sent a request, and all 300 requests sat in line for the lock.
**The profile shows every k6 user stuck in the same line.**

### 1.5 Why is mutex 1.28 h and block 1.66 h?

Do not compare them as exact numbers.

- They were recorded in **different 20 s windows** (09:13:44 and 09:14:04, back to back).
  In the second window the load test may have been finishing, so fewer goroutines were waiting.
- They count at **different moments**. Block records a wait when `Lock` returns.
  Mutex records it when the holder calls `Unlock`. A wait that crosses a window edge can land in one profile but not the other.

What matters: **both point at `ioHandler`**, and both are huge (~230 to ~300 goroutines' worth of waiting).

### 1.6 Other profiles at `http://localhost:8080/debug/pprof/`

| Profile | URL | Use it when |
| --- | --- | --- |
| CPU | `/debug/pprof/profile?seconds=20` | CPU is high. "Which functions burn CPU?" (good for `/cpu`) |
| heap | `/debug/pprof/heap` | Memory is high. "What is in memory right now?" |
| allocs | `/debug/pprof/allocs` | GC is busy. "Who allocates the most?" (Level 4) |
| goroutine | `/debug/pprof/goroutine?debug=1` | "How many goroutines, and where are they?" `debug=1` = grouped counts. `debug=2` = every stack, with its wait reason and wait time |
| block | `/debug/pprof/block?seconds=20` | Latency is high but CPU is low. "Who is waiting?" |
| mutex | `/debug/pprof/mutex?seconds=20` | "Which lock is contended, and who holds it?" |
| threadcreate | `/debug/pprof/threadcreate` | Rare. OS thread count keeps growing |
| trace | `/debug/pprof/trace?seconds=5` | You need a timeline of every goroutine. Open with `go tool trace trace.out` |

Quick goroutine check during load (no tools needed):

```sh
curl -s 'localhost:8080/debug/pprof/goroutine?debug=1' | head -20
```

The first line says `goroutine profile: total N`. Under load at RATE=150, expect around 300.

**Lab setting warning.** `runtime.SetBlockProfileRate(1)` and `runtime.SetMutexProfileFraction(1)` record **every** event.
That costs CPU, and you only have 0.1 CPU. Fine for the lab. In production, use a sampled rate.

### 1.7 Recipe: profile under load

Profiles only see what happens **while** they record. So the load must run at the same time.

```
Terminal 1                               Terminal 2
──────────                               ──────────
make up          (after every code change)
make load ENDPOINT=/io RATE=150 DURATION=1m
  ... load running ...                   wait ~10 s (let the queue fill)
  ... load running ...                   go tool pprof -top \
  ... load running ...                     'http://localhost:8080/debug/pprof/block?seconds=20'
  ... load running ...                   (repeat for mutex, goroutine, ...)
load ends                                read the result
```

Use `DURATION=1m` so the load lasts longer than the 20 s capture.

To open the web UI instead of `-top`:

```sh
go tool pprof -http=:8081 'http://localhost:8080/debug/pprof/block?seconds=20'
```

Pitfalls you already met:

| Symptom | Cause | Fix |
| --- | --- | --- |
| Profile file is 19 bytes | It is the text `404 page not found` (19 bytes). The container runs old code | `make up` after every code change |
| `dot: executable file not found` | Graph view needs Graphviz | `brew install graphviz` |
| `address already in use` | An old `pprof -http=:8081` is still running | Stop it (Ctrl+C), or use another port |

---

## 2. Monitoring

There are three layers. You have the first one now.

### 2.1 Layer A: on-demand pprof (now)

The web UI (`-http=:8081`) has a **View** menu:

- **Top**: same as `-top`, as a sortable table.
- **Graph**: boxes and arrows. Big box = more time. Needs Graphviz.
- **Flame Graph**: the easiest picture.
- **Source**: your code, with time per line. Good for finding the exact line.

How to read the flame graph:

```
[──────────────── net/http.(*conn).serve ────────────────]   caller on top
[──────────────── ServeMux.ServeHTTP ────────────────────]
[──────────────── main.ioHandler ──────────────────────  ]
[──────────────── sync.(*Mutex).Lock ────────────────────]   callee below
```

- **Width** = share of time. Wider = more time.
- Top to bottom = caller to callee (in pprof's view).
- Left to right order means nothing. It is not a timeline.
- Look for the **widest bar near the bottom**. That is where the time goes.

### 2.2 Layer B: continuous metrics (now and later)

pprof answers "**why**". Metrics answer "**when** and **how much**", all the time.

- Now: `make stats` (`docker stats`) shows CPU and memory per container, live.
  Watch the app's CPU stay near 10% (its 0.1 CPU limit) under load.
- k6 live dashboard: watch RPS, latency, and VUs while a test runs.
  The Makefile does not expose the port, so run k6 directly:

  ```sh
  docker compose run --rm -p 5665:5665 -e K6_WEB_DASHBOARD=true \
    -e MODE=ramp -e ENDPOINT=/io -e RATE=500 -e DURATION=1m \
    k6 run /scripts/load.js
  ```

  Open `http://localhost:5665` while it runs. It closes when the test ends.
- Later (recommended): Prometheus + Grafana. Track RPS, p99 latency, in-flight requests, and goroutine count over time.

### 2.3 Layer C: continuous profiling (just so you know)

Tools like **Grafana Pyroscope** and **Parca** take profiles all the time, like metrics.
Then you can ask "what did the CPU look like at 09:13?" after the fact. You do not need them for this lab.

---

## 3. The ceiling

### 3.1 What is the ceiling?

The **ceiling** is the highest request rate the server can handle **while still passing the SLO**:

- p99 < 200 ms
- errors < 0.1%
- 0 dropped iterations

Below the ceiling: requests finish as fast as they arrive.
Above the ceiling: requests arrive **faster** than they finish. A line (queue) grows. Latency explodes.

### 3.2 The cashier analogy

One cashier. Each customer takes 10 ms. So the cashier serves at most **100 customers per second**.

```
arrive 80/s   ──>  [cashier: 100/s max]  ──>  done 80/s     line stays short   PASS
arrive 150/s  ──>  [cashier: 100/s max]  ──>  done ~89/s    line grows ~61/s   FAIL
```

Your `ioHandler` is that cashier. The global lock lets only **one** request work at a time.
Each one holds the lock for ~10 ms. So: `1 s ÷ 10 ms = 100 RPS` max, no matter how many requests arrive.

Your real numbers at RATE=150:

```
target   150 RPS
achieved  ~89 RPS     (the cashier's real speed)
gap       ~61 RPS     join the line every second
line      fills all 300 k6 users in ~5 s (300 ÷ 61)
then      k6 has no free user -> 917 dropped iterations
p99       3.43 s      (time spent standing in line)
```

### 3.3 Little's Law

```
in-flight requests  =  rate  ×  latency
```

Check it with your numbers:

- At 80 RPS: `80 × 0.012 s ≈ 1` request in flight. Almost no line.
- At 150 RPS: 300 requests in flight, served at ~89 RPS.
  `latency = 300 ÷ 89 ≈ 3.4 s`. Your p50 was 3.24 s and p99 3.43 s. It matches.

This is the same 300 you found in the block profile (section 1.4). Three tools, one story:
k6 says 300 VUs busy, pprof says 300 goroutines waiting, Little's Law says 3.4 s per request.

### 3.4 Why ~89, not 100?

100 is the perfect case. Real life adds cost:

- Each request also does real work: parse the request, encode JSON, hand over the lock. So it holds the lock for a bit more than 10 ms.
- The app has 0.1 CPU = 10 ms of CPU per 100 ms. When it uses that up, Linux pauses the process until the next 100 ms window.
  If that pause hits while a goroutine holds the lock, **everyone** waits longer.

### 3.5 The knee

Latency stays flat, then shoots up at the ceiling. This bend is called the **knee**.

```
p99 latency
  3.4 s |                                         x  (150 RPS)
        |                                        /
        |                                       /
        |                                      /
        |                                     /
 200 ms |- - - - - - - - - - - - - - - - - - -/- - - - SLO
 111 ms |  .    .    .    .    .    .    x  /          (80 RPS)
        |                                 |
        +----+----+----+----+----+----+---+----+----> target RPS
            10   20   30   40   50   60  80  ~89  150
                                             ^
                                        knee = ceiling
```

(Only the two `x` points are measured. The line between them is the typical shape.)

Your job in later levels: move the knee to the right.

---

## 4. `make load` vs `make ramp`

### 4.1 `make load`: an exam at one rate

```
RPS
150 |  ┌──────────────────────────┐
    |  │                          │
    |  │                          │
  0 +──┴──────────────────────────┴──> time
       0                       DURATION
```

- k6 executor: `constant-arrival-rate`.
- Holds **exactly** RATE for DURATION, even if the server is slow.
- Result: **PASS or FAIL** at that one rate.

### 4.2 `make ramp`: a search for the region

```
RPS
500 |                              /
    |                           /
    |                        /
    |      the ceiling  ->/   (somewhere on this slope)
 10 |  /
  0 +─────────────────────────────> time
    0                          DURATION
```

- k6 executor: `ramping-arrival-rate`.
- Starts at 10 RPS. Climbs in a straight line to RATE over DURATION.
- Use it to find **roughly where** the server breaks.

At any time `t`, the target rate is:

```
rate(t) = 10 + (RATE - 10) × t ÷ DURATION
```

Example: `RATE=500 DURATION=1m`. Climbs ~8 RPS per second.
If latency jumps at t = 10 s: `10 + 490 × 10 ÷ 60 ≈ 92 RPS`. That is near your ceiling.

### 4.3 Important caveat: the ramp summary hides the break point

The k6 summary at the end **averages the whole ramp**.
It mixes the easy part (10 RPS) with the broken part (500 RPS).
So it will say FAIL, but it will **not** say "it broke at 89 RPS".

To see the break point from a ramp:

- Watch it **live** in the k6 web dashboard (section 2.2). Note the time when latency shoots up, or when achieved RPS stops climbing.
- Or note when **dropped iterations** start, then use the formula above to convert time to RPS.

Also: the queue needs a few seconds to grow, so the visible jump comes a little **after** the real ceiling. Treat the ramp result as a rough guess.

### 4.4 Recommended workflow

```
1. make ramp      -> find the region           ("breaks somewhere near 90")
2. make load ×N   -> pin the exact ceiling     (bisection, see below)
3. profile        -> run load just ABOVE the ceiling, capture block/mutex/CPU
                     to see WHY it breaks
4. fix one thing  -> make up -> back to step 1
```

Bisection with `make load`: you know PASS at 80 and FAIL at 150. Try the middle.

```
80 PASS        150 FAIL
     \          /
      try 115 -> FAIL
80 PASS   115 FAIL
     try 97 -> FAIL
80 PASS   97 FAIL
     try 88 -> ...
```

Stop when the gap is small (about 5 RPS). Run each test twice (Docker on a Mac adds 10-20% noise).

---

## 5. Check yourself

Answer these from your own profile and numbers:

1. Your block profile says 1.66 h in 20 s. How many goroutines were waiting at once, and where does that number come from in your k6 settings?
2. The block profile does not record `time.Sleep`. So what exactly were those goroutines waiting for, and which goroutine were they waiting on (use the mutex profile)?
3. Open the block profile in the web UI and switch to **Source** view for `main.ioHandler`. Which lines run **while the lock is held**? Which of them actually need the lock?
