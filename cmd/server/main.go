// Level 0: a deliberately naive server.
// Each endpoint hides a classic mistake that load tests will expose
// and later levels will fix. Do not copy this into production.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

var (
	// Mistake: one global lock shared by every endpoint.
	mu       sync.Mutex
	requests int

	cpuRounds = envInt("CPU_ROUNDS", 5000)
)

func main() {
	http.HandleFunc("/fast", fastHandler)
	http.HandleFunc("/io", ioHandler)
	http.HandleFunc("/cpu", cpuHandler)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	log.Println("level-0 server listening on :8080")
	// Mistake: no timeouts, no body/header limits, no graceful shutdown.
	// SIGTERM kills the process instantly and drops in-flight requests.
	log.Fatal(http.ListenAndServe(":8080", nil))
}

// fastHandler is the cheapest possible request: bump a counter and reply.
func fastHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	requests++
	n := requests
	mu.Unlock()

	writeJSON(w, map[string]any{"ok": true, "n": n})
}

// ioHandler simulates a 10ms call to a downstream service (DB, another API).
// Mistake: the global lock is held while waiting, so every /io request runs
// one at a time (ceiling = 1s / 10ms = 100 RPS) and /fast queues behind it.
func ioHandler(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	requests++
	time.Sleep(10 * time.Millisecond)
	writeJSON(w, map[string]any{"ok": true, "n": requests})
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
