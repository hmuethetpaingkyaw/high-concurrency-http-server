// Level 3: shop endpoints backed by Postgres and Redis (see shop.go), on top
// of Level 2's overload protection (timeouts, memory limit, per-endpoint
// in-flight caps). Graceful shutdown is still missing.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	_ "net/http/pprof" // registers /debug/pprof/* on http.DefaultServeMux (internal port only)
	"os"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

var (
	// Level 1 fix: the only shared state is one counter, so an atomic
	// replaces the global mutex. No request ever waits for another.
	requests atomic.Int64

	cpuRounds = envInt("CPU_ROUNDS", 5000)
)

// maxBodyBytes caps request bodies. No endpoint reads one yet, but without
// a cap a single client could stream gigabytes into a future handler.
const maxBodyBytes = 1 << 20 // 1 MB

func main() {
	setMemoryLimit()
	runtime.SetBlockProfileRate(1)     // record every time a goroutine blocks
	runtime.SetMutexProfileFraction(1) // record every contended mutex
	// Public routes get their own mux. http.DefaultServeMux, where pprof
	// registers itself, is served only on the internal debug port.
	mux := http.NewServeMux()

	// Level 2: each endpoint has its own in-flight cap (a bulkhead).
	// I/O-bound work mostly waits, so its cap comes from Little's Law: ceiling
	// RPS x latency budget (~8000 x 100ms, leaving room for CPU-quota pauses;
	// a cap of 400 shed 0.16% at 6000 RPS). CPU-bound work gains nothing from
	// running more requests than there are cores; extra ones only queue and
	// slow everyone down, so its cap is GOMAXPROCS.
	mux.HandleFunc("/fast", limit(envInt("MAX_INFLIGHT_FAST", 1000), fastHandler))
	mux.HandleFunc("/io", limit(envInt("MAX_INFLIGHT_IO", 1000), ioHandler))
	mux.HandleFunc("/cpu", limit(envInt("MAX_INFLIGHT_CPU", runtime.GOMAXPROCS(0)), cpuHandler))

	// Level 3: shop endpoints. Each gets its own in-flight cap (MAX_INFLIGHT_DB).
	shop := newShop()
	dbCap := envInt("MAX_INFLIGHT_DB", 1000)
	mux.HandleFunc("GET /products/{id}", limit(dbCap, shop.getProduct))
	mux.HandleFunc("GET /products", limit(dbCap, shop.listProducts))
	mux.HandleFunc("POST /orders", limit(dbCap, shop.createOrder))

	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	// Level 2: every phase of a connection has a deadline, so a slow or idle
	// client can't hold a goroutine and its buffers forever (slowloris).
	srv := &http.Server{
		Addr:              ":8080",
		Handler:           http.MaxBytesHandler(mux, maxBodyBytes),
		ReadHeaderTimeout: 5 * time.Second,  // finish sending headers in 5s or be cut off
		ReadTimeout:       10 * time.Second, // whole request, including body
		WriteTimeout:      15 * time.Second, // handler + writing the response
		IdleTimeout:       60 * time.Second, // keep-alive connections waiting for the next request
		MaxHeaderBytes:    64 << 10,         // 64 KB of headers is plenty; the default is 1 MB
	}

	// Debug server: pprof and /debug/vars (expvar) on an internal port that the outside world can't
	// reach (published only on 127.0.0.1 in compose). No WriteTimeout, because
	// a CPU profile streams for as many seconds as you ask for.
	debugSrv := &http.Server{Addr: ":6060", Handler: http.DefaultServeMux, ReadHeaderTimeout: 5 * time.Second}
	go func() { log.Fatal(debugSrv.ListenAndServe()) }()

	log.Printf("level-3 server listening on :8080 (GOMAXPROCS=%d, memory limit=%d MiB)",
		runtime.GOMAXPROCS(0), debug.SetMemoryLimit(-1)>>20)
	// Mistake: no graceful shutdown. SIGTERM kills the process instantly and
	// drops in-flight requests.
	log.Fatal(srv.ListenAndServe())
}

// fastHandler is the cheapest possible request: bump a counter and reply.
func fastHandler(w http.ResponseWriter, r *http.Request) {
	n := requests.Add(1)
	writeJSON(w, map[string]any{"ok": true, "n": n})
}

// ioHandler simulates a 10ms call to a downstream service (DB, another API).
// Level 0 held the global lock across the wait, so requests ran one at a
// time (ceiling ~100 RPS). Now only the counter update is shared, and the
// value is read once so the reply never touches shared state again.
func ioHandler(w http.ResponseWriter, r *http.Request) {
	n := requests.Add(1)
	time.Sleep(10 * time.Millisecond)
	writeJSON(w, map[string]any{"ok": true, "n": n})
}

// cpuHandler simulates CPU-heavy work such as password hashing.
// Its ceiling is set by the container's CPU quota, not by code bugs.
func cpuHandler(w http.ResponseWriter, r *http.Request) {
	sum := sha256.Sum256([]byte(r.URL.RawQuery))
	for i := 0; i < cpuRounds; i++ {
		sum = sha256.Sum256(sum[:])
	}
	writeJSON(w, map[string]any{"ok": true, "hash": hex.EncodeToString(sum[:])})
}

// limit sheds load: once max requests are in progress, new ones get an
// immediate 503 instead of queuing. Latency and memory stay bounded no matter
// how big the spike is, and clients learn to back off. max <= 0 disables it.
func limit(max int, next http.HandlerFunc) http.HandlerFunc {
	if max <= 0 {
		return next
	}
	slots := make(chan struct{}, max)
	return func(w http.ResponseWriter, r *http.Request) {
		select {
		case slots <- struct{}{}:
			defer func() { <-slots }()
			next(w, r)
		default:
			w.Header().Set("Retry-After", "1")
			http.Error(w, "server busy, retry later", http.StatusServiceUnavailable)
		}
	}
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

// setMemoryLimit tells the GC about the container's memory cap (cgroup v2),
// keeping 10% headroom for stacks and runtime overhead. Near the limit the GC
// runs harder instead of letting the kernel OOM-kill the process.
// An explicit GOMEMLIMIT always wins.
func setMemoryLimit() {
	if os.Getenv("GOMEMLIMIT") != "" {
		return
	}
	b, err := os.ReadFile("/sys/fs/cgroup/memory.max")
	if err != nil {
		return // not running in a cgroup v2 container
	}
	limit, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil {
		return // "max" means no limit
	}
	debug.SetMemoryLimit(limit / 10 * 9)
}

func envDuration(key string, def time.Duration) time.Duration {
	if v, err := time.ParseDuration(os.Getenv(key)); err == nil {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
