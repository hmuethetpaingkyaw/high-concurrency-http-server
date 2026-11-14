// Level 3: shop endpoints backed by Postgres and Redis.
//
// Every fix is behind an env toggle, and the defaults are the naive (step 0)
// behavior, so each fix can be switched on and measured on its own:
//
//	step 1  DB_POOL=on        connection pool instead of a new connection per request
//	step 2  DB_TIMEOUT=300ms  deadline on DB work, tied to the request
//	step 3  LIST_QUERY=join   one query instead of N+1
//	step 4  CACHE=on          Redis cache-aside for GET /products/{id}
//	step 5  SINGLEFLIGHT=on   concurrent cache misses share one DB load
//
// See docs/LEVEL3_GUIDE.md for how to test each step.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"expvar"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/sync/singleflight"
)

// Counters served as JSON on the internal port at /debug/vars (`make vars`).
var (
	dbQueries   = expvar.NewInt("db_queries")
	dbConnects  = expvar.NewInt("db_connects") // new connections opened (pool off: one per request)
	dbErrors    = expvar.NewInt("db_errors")
	dbTimeouts  = expvar.NewInt("db_timeouts")
	dbLastError = expvar.NewString("db_last_error")
	cacheHits   = expvar.NewInt("cache_hits")
	cacheMisses = expvar.NewInt("cache_misses")
	cacheErrors = expvar.NewInt("cache_errors")
	sfShared    = expvar.NewInt("singleflight_shared") // callers whose DB load was shared with others (including the one that ran it)

	idempotentReplays = expvar.NewInt("idempotent_replays") // POST /orders answered from an earlier order with the same key
)

// maxKeyLen is the longest accepted Idempotency-Key.
const maxKeyLen = 128

// querier is what handlers need from a connection. Both a one-off *pgx.Conn
// and a pooled *pgxpool.Conn satisfy it.
type querier interface {
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	Begin(ctx context.Context) (pgx.Tx, error)
}

// product fields are in the same order as the SELECT columns below.
type product struct {
	ID          int    `json:"id"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	PriceCents  int    `json:"price_cents"`
	Description string `json:"description"`
	Stock       int    `json:"stock"`
}

type shop struct {
	dbURL     string
	pool      *pgxpool.Pool // nil when DB_POOL=off
	dbTimeout time.Duration // 0 = no timeout
	listJoin  bool
	cache     *redis.Client // nil when CACHE=off
	cacheTTL  time.Duration
	sf        *singleflight.Group // nil when SINGLEFLIGHT=off

	idempotent bool // IDEMPOTENCY=on: honor the Idempotency-Key header on POST /orders
}

func newShop() *shop {
	s := &shop{
		dbURL:      os.Getenv("DATABASE_URL"),
		dbTimeout:  envDuration("DB_TIMEOUT", 0),
		listJoin:   os.Getenv("LIST_QUERY") == "join",
		cacheTTL:   envDuration("CACHE_TTL", 30*time.Second),
		idempotent: os.Getenv("IDEMPOTENCY") == "on",
	}

	if os.Getenv("DB_POOL") == "on" {
		cfg, err := pgxpool.ParseConfig(s.dbURL)
		if err != nil {
			log.Fatalf("DATABASE_URL: %v", err)
		}
		cfg.MaxConns = int32(envInt("DB_MAX_CONNS", 10))
		// MinConns connections are opened at startup and kept open, so early
		// requests don't wait for a brand-new connection (0 = open on demand).
		cfg.MinConns = int32(envInt("DB_MIN_CONNS", 0))
		cfg.AfterConnect = func(context.Context, *pgx.Conn) error {
			dbConnects.Add(1)
			return nil
		}
		if s.pool, err = pgxpool.NewWithConfig(context.Background(), cfg); err != nil {
			log.Fatalf("db pool: %v", err)
		}
		expvar.Publish("db_pool", expvar.Func(func() any {
			st := s.pool.Stat()
			return map[string]any{
				"max":           st.MaxConns(),
				"total":         st.TotalConns(),
				"in_use":        st.AcquiredConns(),
				"idle":          st.IdleConns(),
				"acquires":      st.AcquireCount(),
				"had_to_wait":   st.EmptyAcquireCount(), // acquires that found no idle connection
				"wait_total_ms": st.AcquireDuration().Milliseconds(),
			}
		}))
	}

	if os.Getenv("CACHE") == "on" {
		s.cache = redis.NewClient(&redis.Options{
			Addr:         os.Getenv("REDIS_ADDR"),
			DialTimeout:  time.Second,
			ReadTimeout:  200 * time.Millisecond,
			WriteTimeout: 200 * time.Millisecond,
		})
	}

	if os.Getenv("SINGLEFLIGHT") == "on" {
		s.sf = &singleflight.Group{}
	}

	expvar.Publish("level3_config", expvar.Func(func() any {
		return map[string]any{
			"DB_POOL":      s.pool != nil,
			"DB_MAX_CONNS": envInt("DB_MAX_CONNS", 10),
			"DB_MIN_CONNS": envInt("DB_MIN_CONNS", 0),
			"DB_TIMEOUT":   s.dbTimeout.String(),
			"LIST_QUERY":   map[bool]string{true: "join", false: "n+1"}[s.listJoin],
			"CACHE":        s.cache != nil,
			"CACHE_TTL":    s.cacheTTL.String(),
			"SINGLEFLIGHT": s.sf != nil,
		}
	}))
	expvar.Publish("level7_config", expvar.Func(func() any {
		return map[string]any{"IDEMPOTENCY": s.idempotent}
	}))
	return s
}

// dbContext is the context for one request's DB work. The naive version
// (DB_TIMEOUT=0) uses context.Background(): queries keep running, and keep
// holding a connection, even after the client has given up. With DB_TIMEOUT
// the work is tied to the request and has a deadline.
func (s *shop) dbContext(r *http.Request) (context.Context, context.CancelFunc) {
	if s.dbTimeout <= 0 {
		return context.Background(), func() {}
	}
	return context.WithTimeout(r.Context(), s.dbTimeout)
}

// detachedContext is for work shared by many requests (singleflight): one
// caller giving up must not cancel it for the others.
func (s *shop) detachedContext() (context.Context, context.CancelFunc) {
	if s.dbTimeout <= 0 {
		return context.Background(), func() {}
	}
	return context.WithTimeout(context.Background(), s.dbTimeout)
}

// conn returns a connection and a function that gives it back. With the pool
// off, every request opens a brand-new connection: TCP handshake, password
// auth and a new Postgres backend process each time, with no upper bound on
// how many exist at once (Postgres allows 100).
func (s *shop) conn(ctx context.Context) (querier, func(), error) {
	if s.pool != nil {
		c, err := s.pool.Acquire(ctx)
		if err != nil {
			return nil, nil, err
		}
		return c, c.Release, nil
	}
	c, err := pgx.Connect(ctx, s.dbURL)
	if err != nil {
		return nil, nil, err
	}
	dbConnects.Add(1)
	return c, func() { c.Close(context.Background()) }, nil
}

// getProduct serves GET /products/{id}.
func (s *shop) getProduct(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id <= 0 {
		http.Error(w, "bad product id", http.StatusBadRequest)
		return
	}
	key := productKey(id)

	// Step 4: cache-aside. Try Redis first; on a miss, load from Postgres and
	// store the result. If Redis is down, fall through to the DB.
	if s.cache != nil {
		b, err := s.cache.Get(r.Context(), key).Bytes()
		switch {
		case err == nil:
			cacheHits.Add(1)
			writeRawJSON(w, b)
			return
		case errors.Is(err, redis.Nil):
			cacheMisses.Add(1)
		default:
			cacheErrors.Add(1)
		}
	}

	var b []byte
	if s.sf != nil {
		// Step 5: when a hot key expires, hundreds of requests miss at once.
		// singleflight lets one of them load it while the rest wait for
		// that same result instead of all hitting Postgres.
		v, loadErr, shared := s.sf.Do(key, func() (any, error) {
			ctx, cancel := s.detachedContext()
			defer cancel()
			return s.loadProduct(ctx, id)
		})
		if shared {
			sfShared.Add(1)
		}
		b, _ = v.([]byte) // v is nil when the load failed
		err = loadErr
	} else {
		ctx, cancel := s.dbContext(r)
		defer cancel()
		b, err = s.loadProduct(ctx, id)
	}
	if err != nil {
		dbError(w, err)
		return
	}
	writeRawJSON(w, b)
}

// loadProduct reads one product from Postgres and, with the cache on,
// stores the JSON in Redis.
func (s *shop) loadProduct(ctx context.Context, id int) ([]byte, error) {
	c, release, err := s.conn(ctx)
	if err != nil {
		return nil, err
	}
	defer release()

	var p product
	dbQueries.Add(1)
	err = c.QueryRow(ctx, `
		SELECT p.id, p.name, p.category, p.price_cents, p.description, i.stock
		FROM products p JOIN inventory i ON i.product_id = p.id
		WHERE p.id = $1`, id).
		Scan(&p.ID, &p.Name, &p.Category, &p.PriceCents, &p.Description, &p.Stock)
	if err != nil {
		return nil, err
	}
	b, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	if s.cache != nil {
		if err := s.cache.Set(ctx, productKey(id), b, s.cacheTTL).Err(); err != nil {
			cacheErrors.Add(1)
		}
	}
	return b, nil
}

// listProducts serves GET /products?category=cat-7&limit=20.
func (s *shop) listProducts(w http.ResponseWriter, r *http.Request) {
	category := r.URL.Query().Get("category")
	if category == "" {
		http.Error(w, "category is required", http.StatusBadRequest)
		return
	}
	limit := 20
	if n, err := strconv.Atoi(r.URL.Query().Get("limit")); err == nil && n > 0 && n <= 100 {
		limit = n
	}

	ctx, cancel := s.dbContext(r)
	defer cancel()
	c, release, err := s.conn(ctx)
	if err != nil {
		dbError(w, err)
		return
	}
	defer release()

	var items []product
	if s.listJoin {
		// Step 3: one query; Postgres joins the stock in.
		dbQueries.Add(1)
		rows, err := c.Query(ctx, `
			SELECT p.id, p.name, p.category, p.price_cents, p.description, i.stock
			FROM products p JOIN inventory i ON i.product_id = p.id
			WHERE p.category = $1 ORDER BY p.id LIMIT $2`, category, limit)
		if err == nil {
			items, err = pgx.CollectRows(rows, pgx.RowToStructByPos[product])
		}
		if err != nil {
			dbError(w, err)
			return
		}
	} else {
		// Step 0: N+1. One query for the page, then one more per product:
		// 21 round trips for a page of 20.
		dbQueries.Add(1)
		rows, err := c.Query(ctx, `
			SELECT id, name, category, price_cents, description, 0
			FROM products WHERE category = $1 ORDER BY id LIMIT $2`, category, limit)
		if err == nil {
			items, err = pgx.CollectRows(rows, pgx.RowToStructByPos[product])
		}
		if err != nil {
			dbError(w, err)
			return
		}
		for i := range items {
			dbQueries.Add(1)
			err := c.QueryRow(ctx, `SELECT stock FROM inventory WHERE product_id = $1`, items[i].ID).
				Scan(&items[i].Stock)
			if err != nil {
				dbError(w, err)
				return
			}
		}
	}
	writeJSON(w, items)
}

// idempotencyKey returns the request's Idempotency-Key. ok is false when the
// header is present but invalid: empty after trimming, longer than maxKeyLen,
// not printable ASCII, or sent more than once. A missing header is fine
// ("", true), and with IDEMPOTENCY off the header is ignored.
func (s *shop) idempotencyKey(r *http.Request) (key string, ok bool) {
	if !s.idempotent {
		return "", true
	}
	vals, present := r.Header["Idempotency-Key"]
	if !present {
		return "", true
	}
	if len(vals) != 1 {
		return "", false
	}
	key = strings.TrimSpace(vals[0])
	if key == "" || len(key) > maxKeyLen {
		return "", false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x20 || key[i] > 0x7e {
			return "", false
		}
	}
	return key, true
}

// createOrder serves POST /orders {"product_id": 42, "qty": 1}.
func (s *shop) createOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ProductID int `json:"product_id"`
		Qty       int `json:"qty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil || req.ProductID <= 0 || req.Qty < 1 || req.Qty > 100 {
		http.Error(w, `body must be {"product_id": >0, "qty": 1-100}`, http.StatusBadRequest)
		return
	}
	// Level 7: with IDEMPOTENCY=on, a present but invalid key is rejected
	// before any database work. With it off the header is ignored.
	key, ok := s.idempotencyKey(r)
	if !ok {
		http.Error(w, "invalid Idempotency-Key (1-128 printable ASCII characters, one value)", http.StatusBadRequest)
		return
	}

	ctx, cancel := s.dbContext(r)
	defer cancel()
	c, release, err := s.conn(ctx)
	if err != nil {
		dbError(w, err)
		return
	}
	defer release()

	// Level 7: a request with a key (switch on) takes its own path. Without a
	// key, or with the switch off, the original flow below is unchanged.
	if key != "" {
		s.createOrderWithKey(ctx, w, c, req.ProductID, req.Qty, key)
		return
	}

	tx, err := c.Begin(ctx)
	if err != nil {
		dbError(w, err)
		return
	}
	defer tx.Rollback(context.Background()) // no-op after a successful Commit

	// The UPDATE locks this product's inventory row until commit, so
	// concurrent orders for the same product run one at a time: Level 0's
	// global lock again, this time inside the database.
	dbQueries.Add(1)
	tag, err := tx.Exec(ctx, `UPDATE inventory SET stock = stock - $2 WHERE product_id = $1 AND stock >= $2`,
		req.ProductID, req.Qty)
	if err != nil {
		dbError(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		http.Error(w, "unknown product or out of stock", http.StatusConflict)
		return
	}
	var orderID int64
	dbQueries.Add(1)
	if err := tx.QueryRow(ctx, `INSERT INTO orders (product_id, qty) VALUES ($1, $2) RETURNING id`,
		req.ProductID, req.Qty).Scan(&orderID); err != nil {
		dbError(w, err)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		dbError(w, err)
		return
	}

	// Stock changed, so the cached copy is stale: delete it and let the next
	// read reload it (cache-aside invalidation).
	if s.cache != nil {
		if err := s.cache.Del(ctx, productKey(req.ProductID)).Err(); err != nil {
			cacheErrors.Add(1)
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(map[string]any{"order_id": orderID, "product_id": req.ProductID, "qty": req.Qty})
}

func productKey(id int) string { return "product:" + strconv.Itoa(id) }

// dbError maps a DB error to a status code and counts it.
func dbError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, context.DeadlineExceeded) || pgconn.Timeout(err):
		dbTimeouts.Add(1)
		http.Error(w, "database timeout", http.StatusGatewayTimeout)
	default:
		dbErrors.Add(1)
		dbLastError.Set(err.Error())
		http.Error(w, "database error", http.StatusInternalServerError)
	}
}

// createOrderWithKey is createOrder for a request with an Idempotency-Key.
//
// The order is inserted first, with its key, and the unique index on the key
// decides: if another transaction already committed an order with this key,
// the insert does nothing and we answer with that order (a replay). Only a
// request whose insert succeeded takes the stock, so a replay never takes it
// twice. If the stock step fails, the rollback also removes the key, so the
// client may retry later. Two requests with the same key at the same time:
// the second insert waits for the first transaction to finish, then replays
// (or proceeds, if the first one rolled back).
func (s *shop) createOrderWithKey(ctx context.Context, w http.ResponseWriter, c querier, productID, qty int, key string) {
	tx, err := c.Begin(ctx)
	if err != nil {
		dbError(w, err)
		return
	}
	defer tx.Rollback(context.Background()) // no-op after a successful Commit

	var orderID int64
	dbQueries.Add(1)
	err = tx.QueryRow(ctx, `INSERT INTO orders (product_id, qty, idempotency_key) VALUES ($1, $2, $3)
		ON CONFLICT (idempotency_key) DO NOTHING RETURNING id`, productID, qty, key).Scan(&orderID)
	if errors.Is(err, pgx.ErrNoRows) {
		// Replay: an order with this key is already committed.
		var prevProduct, prevQty int
		dbQueries.Add(1)
		if err := tx.QueryRow(ctx, `SELECT id, product_id, qty FROM orders WHERE idempotency_key = $1`, key).
			Scan(&orderID, &prevProduct, &prevQty); err != nil {
			dbError(w, err)
			return
		}
		if prevProduct != productID || prevQty != qty {
			http.Error(w, "idempotency key was used with a different request", http.StatusUnprocessableEntity)
			return
		}
		idempotentReplays.Add(1)
		w.Header().Set("Idempotent-Replay", "true")
		writeOrder(w, http.StatusOK, orderID, productID, qty)
		return
	}
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23503" { // foreign key: no such product
			http.Error(w, "unknown product or out of stock", http.StatusConflict)
			return
		}
		dbError(w, err)
		return
	}

	dbQueries.Add(1)
	tag, err := tx.Exec(ctx, `UPDATE inventory SET stock = stock - $2 WHERE product_id = $1 AND stock >= $2`,
		productID, qty)
	if err != nil {
		dbError(w, err)
		return
	}
	if tag.RowsAffected() == 0 {
		// The deferred rollback also undoes the insert, so the key is not stored.
		http.Error(w, "unknown product or out of stock", http.StatusConflict)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		dbError(w, err)
		return
	}

	if s.cache != nil {
		if err := s.cache.Del(ctx, productKey(productID)).Err(); err != nil {
			cacheErrors.Add(1)
		}
	}
	writeOrder(w, http.StatusCreated, orderID, productID, qty)
}

func writeOrder(w http.ResponseWriter, status int, orderID int64, productID, qty int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"order_id": orderID, "product_id": productID, "qty": qty})
}

func writeRawJSON(w http.ResponseWriter, b []byte) {
	w.Header().Set("Content-Type", "application/json")
	w.Write(b)
}

// close releases the Postgres pool and the Redis client. Called at the end of a
// graceful shutdown, after the HTTP server has finished its in-flight requests.
//
// pgxpool.Close waits until every borrowed connection is returned. If the
// database is frozen, a hung query never returns its connection, so Close
// would block until the database comes back (seen in Level 6 step 3: 20.6 s).
// So the wait is bounded: after timeout we give up and let the process exit,
// which closes the sockets anyway.
func (s *shop) close(timeout time.Duration) {
	if s.pool != nil {
		done := make(chan struct{})
		go func() {
			s.pool.Close()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(timeout):
			log.Printf("shutdown: pool still has busy connections after %s, giving up", timeout)
		}
	}
	if s.cache != nil {
		s.cache.Close()
	}
}
