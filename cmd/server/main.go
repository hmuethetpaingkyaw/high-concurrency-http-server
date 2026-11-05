// Level 1: the global lock is gone; requests no longer wait on each other.
// The server-wide mistakes below remain and later levels will fix them.
// Do not copy this into production.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	_ "net/http/pprof" // lab-only shortcut: exposes /debug/pprof/* on the public port
	"os"
	"runtime"
	"strconv"
	"sync/atomic"
	"time"
)

var (
	// Level 1 fix: the only shared state is one counter, so an atomic
	// replaces the global mutex. No request ever waits for another.
	requests atomic.Int64

	cpuRounds = envInt("CPU_ROUNDS", 5000)
)

func main() {
	runtime.SetBlockProfileRate(1)     // record every time a goroutine blocks
	runtime.SetMutexProfileFraction(1) // record every contended mutex
	http.HandleFunc("/fast", fastHandler)
	http.HandleFunc("/io", ioHandler)
	http.HandleFunc("/cpu", cpuHandler)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Println("level-1 server listening on :8080")
	// Mistake: no timeouts, no body/header limits, no graceful shutdown.
	// SIGTERM kills the process instantly and drops in-flight requests.
	log.Fatal(http.ListenAndServe(":8080", nil))
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

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		log.Printf("write response: %v", err)
	}
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
