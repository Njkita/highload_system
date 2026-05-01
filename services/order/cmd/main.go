package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type config struct {
	httpAddr      string
	pgDSN         string
	redisAddr     string
	catalogURL    string
	cacheEnabled  bool
	pgMaxConns    int32
	rateRPS       int
	rateBurst     int
}

func loadConfig() config {
	return config{
		httpAddr:     getenv("HTTP_ADDR", ":8082"),
		pgDSN:        getenv("PG_DSN", "postgres://food:food@postgres:5432/fooddelivery?sslmode=disable"),
		redisAddr:    getenv("REDIS_ADDR", "redis:6379"),
		catalogURL:   getenv("CATALOG_URL", "http://catalog:8081"),
		cacheEnabled: getenv("CACHE_ENABLED", "false") == "true",
		pgMaxConns:   int32(parseInt(getenv("PG_MAX_CONNS", "4"), 4)),
		rateRPS:      parseInt(getenv("RATE_RPS", "100"), 100),
		rateBurst:    parseInt(getenv("RATE_BURST", "200"), 200),
	}
}

type orderSvc struct {
	db   *pgxpool.Pool
	rdb  *redis.Client
	http *http.Client
	cfg  config
	log  *slog.Logger
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()

	ctx := context.Background()
	pgCfg, err := pgxpool.ParseConfig(cfg.pgDSN)
	must(err, log, "parse pg dsn")
	pgCfg.MaxConns = cfg.pgMaxConns
	pgCfg.MinConns = 1
	pool, err := pgxpool.NewWithConfig(ctx, pgCfg)
	must(err, log, "connect pg")
	defer pool.Close()

	rdb := redis.NewClient(&redis.Options{Addr: cfg.redisAddr, PoolSize: 8})
	defer rdb.Close()

	s := &orderSvc{
		db:  pool,
		rdb: rdb,
		http: &http.Client{
			// timeout пересекает чтение тела — подходит для маленького ответа catalog'а
			Timeout: 500 * time.Millisecond,
		},
		cfg: cfg,
		log: log,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /api/v1/orders", s.rateLimit(s.createOrder))
	mux.HandleFunc("GET /api/v1/orders/{id}", s.getOrder)

	srv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		WriteTimeout:      8 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Info("order up", "addr", cfg.httpAddr, "cache", cfg.cacheEnabled, "pg_max_conns", cfg.pgMaxConns,
		"rate_rps", cfg.rateRPS, "rate_burst", cfg.rateBurst)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

func (s *orderSvc) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 800*time.Millisecond)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		http.Error(w, "db not ready", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

// ---------- rate limit (token bucket в Redis) ----------

// Подход: один ключ на subject (для PoC subject = X-User-Id или IP), TTL = 1s,
// атомарный INCR через Lua. Burst — потолок INCR'а. Когда RATE_RPS=0 — ограничения нет.
//
// Это не Tier-1 решение для прод (пиковое окно 1с грубовато), но для PoC показывает
// сам паттерн: write-операция огорожена rate limiter'ом, чтобы спайк клиента не уронил БД.
const rateLuaScript = `
local v = redis.call('INCR', KEYS[1])
if v == 1 then redis.call('PEXPIRE', KEYS[1], 1000) end
return v`

var rateScript = redis.NewScript(rateLuaScript)

func (s *orderSvc) rateLimit(next http.HandlerFunc) http.HandlerFunc {
	if s.cfg.rateRPS <= 0 {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		subject := r.Header.Get("X-User-Id")
		if subject == "" {
			subject = r.RemoteAddr
		}
		ctx, cancel := context.WithTimeout(r.Context(), 50*time.Millisecond)
		defer cancel()
		key := "rate:order:" + subject + ":" + strconv.FormatInt(time.Now().Unix(), 10)
		v, err := rateScript.Run(ctx, s.rdb, []string{key}).Int()
		if err != nil {
			// fail-open: при недоступности Redis не блокируем checkout
			next(w, r)
			return
		}
		if v > s.cfg.rateBurst {
			w.Header().Set("Retry-After", "1")
			writeProblem(w, http.StatusTooManyRequests, "RATE_LIMITED", "")
			return
		}
		next(w, r)
	}
}

// ---------- createOrder ----------

type orderItemReq struct {
	MenuItemID string   `json:"menu_item_id"`
	Quantity   int      `json:"quantity"`
	Options    []string `json:"options,omitempty"`
}

type addressReq struct {
	City       string  `json:"city"`
	StreetLine string  `json:"street_line"`
	Lat        float64 `json:"lat"`
	Lon        float64 `json:"lon"`
}

type createOrderReq struct {
	RestaurantID    string         `json:"restaurant_id"`
	Items           []orderItemReq `json:"items"`
	DeliveryAddress addressReq     `json:"delivery_address"`
	Comment         string         `json:"comment,omitempty"`
}

type orderItemResp struct {
	MenuItemID string `json:"menu_item_id"`
	Name       string `json:"name"`
	Quantity   int    `json:"quantity"`
	UnitPrice  int    `json:"unit_price"`
}

type createOrderResp struct {
	OrderID      string          `json:"order_id"`
	Status       string          `json:"status"`
	Items        []orderItemResp `json:"items"`
	TotalAmount  int             `json:"total_amount"`
	DeliveryFee  int             `json:"delivery_fee"`
	Currency     string          `json:"currency"`
	CreatedAt    time.Time       `json:"created_at"`
}

func (s *orderSvc) createOrder(w http.ResponseWriter, r *http.Request) {
	idemKey := r.Header.Get("Idempotency-Key")
	if _, err := uuid.Parse(idemKey); err != nil {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "Idempotency-Key должен быть UUID")
		return
	}

	// 1) кэшированный ответ для того же ключа
	if v, err := s.rdb.Get(r.Context(), "idem:order:"+idemKey).Bytes(); err == nil {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Idempotent-Replay", "1")
		w.WriteHeader(http.StatusOK)
		w.Write(v)
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 64*1024))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "")
		return
	}
	var req createOrderReq
	if err := json.Unmarshal(body, &req); err != nil {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "")
		return
	}
	if req.RestaurantID == "" || len(req.Items) == 0 {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "restaurant_id и items обязательны")
		return
	}

	// 2) тянем меню ресторана через catalog-service (Timeout pattern)
	card, err := s.fetchRestaurant(r.Context(), req.RestaurantID)
	if err != nil {
		s.log.Error("catalog call", "err", err)
		writeProblem(w, http.StatusServiceUnavailable, "CATALOG_UNAVAILABLE", err.Error())
		return
	}
	prices := map[string]int{}
	names := map[string]string{}
	for _, m := range card.Menu {
		prices[m.ID] = m.Price
		names[m.ID] = m.Name
	}

	// 3) расчёт суммы по фактическим ценам каталога; снимок в orders.items_snapshot
	respItems := make([]orderItemResp, 0, len(req.Items))
	total := 0
	for _, it := range req.Items {
		p, ok := prices[it.MenuItemID]
		if !ok {
			writeProblem(w, http.StatusUnprocessableEntity, "MENU_ITEM_NOT_FOUND", it.MenuItemID)
			return
		}
		if it.Quantity < 1 || it.Quantity > 50 {
			writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "quantity 1..50")
			return
		}
		respItems = append(respItems, orderItemResp{
			MenuItemID: it.MenuItemID, Name: names[it.MenuItemID], Quantity: it.Quantity, UnitPrice: p,
		})
		total += p * it.Quantity
	}
	deliveryFee := 199
	total += deliveryFee

	// 4) одна транзакция: orders + outbox_events
	orderID := uuid.New()
	now := time.Now().UTC()

	tx, err := s.db.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		s.log.Error("begin tx", "err", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}
	defer tx.Rollback(context.Background())

	itemsSnapshot, _ := json.Marshal(respItems)
	addr, _ := json.Marshal(req.DeliveryAddress)

	_, err = tx.Exec(r.Context(),
		`INSERT INTO orders (id, user_id, restaurant_id, status, items_snapshot,
		                     total_amount, delivery_fee, delivery_address, idempotency_key,
		                     created_at, updated_at)
		 VALUES ($1, $2, $3, 'awaiting_payment', $4, $5, $6, $7, $8, $9, $9)`,
		orderID, anonUserID(r), req.RestaurantID, itemsSnapshot,
		total, deliveryFee, addr, idemKey, now)
	if err != nil {
		// gracefully обработаем гонку идемпотентности
		s.log.Error("insert order", "err", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}

	eventPayload, _ := json.Marshal(map[string]any{
		"order_id":      orderID.String(),
		"restaurant_id": req.RestaurantID,
		"total_amount":  total,
	})
	_, err = tx.Exec(r.Context(),
		`INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, payload)
		 VALUES ('order', $1, 'order.created', $2)`,
		orderID, eventPayload)
	if err != nil {
		s.log.Error("insert outbox", "err", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		s.log.Error("commit tx", "err", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}

	resp := createOrderResp{
		OrderID:     orderID.String(),
		Status:      "awaiting_payment",
		Items:       respItems,
		TotalAmount: total,
		DeliveryFee: deliveryFee,
		Currency:    "RUB",
		CreatedAt:   now,
	}
	respBody, _ := json.Marshal(resp)
	_ = s.rdb.Set(r.Context(), "idem:order:"+idemKey, respBody, 24*time.Hour).Err()

	// hot cache для GET /orders/{id} — обновим, когда iter-2 включит чтение из Redis
	if s.cfg.cacheEnabled {
		_ = s.rdb.Set(r.Context(), "order_hot:"+orderID.String(), respBody, time.Hour).Err()
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	w.Write(respBody)
}

// ---------- catalog client ----------

type menuItemDTO struct {
	ID    string `json:"menu_item_id"`
	Name  string `json:"name"`
	Price int    `json:"price"`
}

type restaurantCardDTO struct {
	Menu []menuItemDTO `json:"menu"`
}

func (s *orderSvc) fetchRestaurant(ctx context.Context, id string) (*restaurantCardDTO, error) {
	ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "GET", s.cfg.catalogURL+"/api/v1/restaurants/"+id, nil)
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("restaurant_not_found")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog status %d", resp.StatusCode)
	}
	var dto restaurantCardDTO
	if err := json.NewDecoder(resp.Body).Decode(&dto); err != nil {
		return nil, err
	}
	return &dto, nil
}

// ---------- getOrder ----------

func (s *orderSvc) getOrder(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := uuid.Parse(id); err != nil {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "")
		return
	}

	if s.cfg.cacheEnabled {
		if v, err := s.rdb.Get(r.Context(), "order_hot:"+id).Bytes(); err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write(v)
			return
		}
	}

	var (
		status, addressJSON, itemsJSON string
		total, fee                     int
		createdAt, updatedAt           time.Time
	)
	err := s.db.QueryRow(r.Context(),
		`SELECT status, items_snapshot::text, total_amount, delivery_fee, delivery_address::text, created_at, updated_at
		   FROM orders WHERE id = $1`, id).
		Scan(&status, &itemsJSON, &total, &fee, &addressJSON, &createdAt, &updatedAt)
	if err != nil {
		writeProblem(w, http.StatusNotFound, "ORDER_NOT_FOUND", "")
		return
	}

	resp := map[string]any{
		"order_id":         id,
		"status":           status,
		"total_amount":     total,
		"delivery_fee":     fee,
		"items":            json.RawMessage(itemsJSON),
		"delivery_address": json.RawMessage(addressJSON),
		"created_at":       createdAt,
		"updated_at":       updatedAt,
	}
	body, _ := json.Marshal(resp)

	if s.cfg.cacheEnabled {
		_ = s.rdb.Set(r.Context(), "order_hot:"+id, body, time.Hour).Err()
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.Write(body)
}

// ---------- helpers ----------

func anonUserID(r *http.Request) uuid.UUID {
	if v := r.Header.Get("X-User-Id"); v != "" {
		if u, err := uuid.Parse(v); err == nil {
			return u
		}
	}
	// для PoC анонимный пользователь — детерминированный по IP
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("anon:"+r.RemoteAddr))
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseInt(s string, def int) int {
	v, err := strconv.Atoi(s)
	if err != nil {
		return def
	}
	return v
}

type problem struct {
	Type   string `json:"type"`
	Title  string `json:"title"`
	Status int    `json:"status"`
	Code   string `json:"code"`
	Detail string `json:"detail,omitempty"`
}

func writeProblem(w http.ResponseWriter, status int, code, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(problem{
		Type:   "about:blank",
		Title:  http.StatusText(status),
		Status: status,
		Code:   code,
		Detail: detail,
	})
}

func must(err error, log *slog.Logger, msg string) {
	if err != nil {
		log.Error(msg, "err", err)
		os.Exit(1)
	}
}
