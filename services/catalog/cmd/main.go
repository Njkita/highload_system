package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

type config struct {
	httpAddr     string
	pgDSN        string
	redisAddr    string
	cacheEnabled bool
	cacheTTL     time.Duration
	pgMaxConns   int32
}

func loadConfig() config {
	c := config{
		httpAddr:     getenv("HTTP_ADDR", ":8081"),
		pgDSN:        getenv("PG_DSN", "postgres://food:food@postgres:5432/fooddelivery?sslmode=disable"),
		redisAddr:    getenv("REDIS_ADDR", "redis:6379"),
		cacheEnabled: getenv("CACHE_ENABLED", "false") == "true",
		cacheTTL:     parseDur(getenv("CACHE_TTL", "60s"), 60*time.Second),
		pgMaxConns:   int32(parseInt(getenv("PG_MAX_CONNS", "4"), 4)),
	}
	return c
}

type catalog struct {
	db    *pgxpool.Pool
	rdb   *redis.Client
	cfg   config
	log   *slog.Logger
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	cfg := loadConfig()

	ctx := context.Background()

	pgCfg, err := pgxpool.ParseConfig(cfg.pgDSN)
	must(err, log, "parse pg dsn")
	pgCfg.MaxConns = cfg.pgMaxConns
	pgCfg.MinConns = 1
	pgCfg.MaxConnLifetime = 30 * time.Minute
	pgCfg.MaxConnIdleTime = 5 * time.Minute

	pool, err := pgxpool.NewWithConfig(ctx, pgCfg)
	must(err, log, "connect pg")
	defer pool.Close()

	var rdb *redis.Client
	if cfg.cacheEnabled {
		rdb = redis.NewClient(&redis.Options{Addr: cfg.redisAddr, PoolSize: 8})
		defer rdb.Close()
	}

	c := &catalog{db: pool, rdb: rdb, cfg: cfg, log: log}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", c.ready)
	mux.HandleFunc("GET /api/v1/restaurants", c.searchRestaurants)
	mux.HandleFunc("GET /api/v1/restaurants/{id}", c.getRestaurant)

	srv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		WriteTimeout:      5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Info("catalog up", "addr", cfg.httpAddr, "cache", cfg.cacheEnabled, "pg_max_conns", cfg.pgMaxConns)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

func (c *catalog) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 800*time.Millisecond)
	defer cancel()
	if err := c.db.Ping(ctx); err != nil {
		http.Error(w, "db not ready", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

type restaurant struct {
	ID           string   `json:"restaurant_id"`
	Name         string   `json:"name"`
	Cuisine      []string `json:"cuisine"`
	Rating       float64  `json:"rating"`
	ETAMinutes   int      `json:"eta_minutes"`
	DeliveryFee  int      `json:"delivery_fee"`
	MinOrder     int      `json:"min_order_amount"`
	Distance     int      `json:"distance_meters,omitempty"`
	IsOpen       bool     `json:"is_open"`
	PreviewURL   string   `json:"preview_photo_url,omitempty"`
	Lat          float64  `json:"-"`
	Lon          float64  `json:"-"`
}

type listResp struct {
	Total   int          `json:"total"`
	Page    int          `json:"page"`
	PerPage int          `json:"per_page"`
	Items   []restaurant `json:"items"`
}

func (c *catalog) searchRestaurants(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	lat := parseFloat(q.Get("lat"), math.NaN())
	lon := parseFloat(q.Get("lon"), math.NaN())
	if math.IsNaN(lat) || math.IsNaN(lon) {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "lat и lon обязательны")
		return
	}
	radius := parseInt(q.Get("radius"), 5000)
	if radius < 100 || radius > 50_000 {
		radius = 5000
	}
	page := parseInt(q.Get("page"), 1)
	if page < 1 {
		page = 1
	}
	perPage := parseInt(q.Get("per_page"), 20)
	if perPage < 1 || perPage > 50 {
		perPage = 20
	}

	cuisineFilter := splitCSV(q.Get("cuisine"))

	cacheKey := ""
	if c.cfg.cacheEnabled {
		cacheKey = "cat:search:" + roundCoord(lat) + ":" + roundCoord(lon) + ":" +
			strconv.Itoa(radius) + ":" + strings.Join(cuisineFilter, ",") + ":" +
			strconv.Itoa(page) + ":" + strconv.Itoa(perPage)
		if v, err := c.rdb.Get(r.Context(), cacheKey).Bytes(); err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write(v)
			return
		}
	}

	var (
		rows pgx.Rows
		err  error
	)
	if len(cuisineFilter) == 0 {
		rows, err = c.db.Query(r.Context(),
			`SELECT id, name, cuisine, rating, eta_minutes, delivery_fee, min_order, is_open, lat, lon, COALESCE(preview_url,'')
			   FROM restaurants
			  WHERE is_open = TRUE
			  LIMIT $1 OFFSET $2`,
			perPage, (page-1)*perPage)
	} else {
		rows, err = c.db.Query(r.Context(),
			`SELECT id, name, cuisine, rating, eta_minutes, delivery_fee, min_order, is_open, lat, lon, COALESCE(preview_url,'')
			   FROM restaurants
			  WHERE is_open = TRUE
			    AND cuisine && $3
			  LIMIT $1 OFFSET $2`,
			perPage, (page-1)*perPage, cuisineFilter)
	}
	if err != nil {
		c.log.Error("query restaurants", "err", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}
	defer rows.Close()

	items := make([]restaurant, 0, perPage)
	for rows.Next() {
		var rt restaurant
		if err := rows.Scan(&rt.ID, &rt.Name, &rt.Cuisine, &rt.Rating, &rt.ETAMinutes,
			&rt.DeliveryFee, &rt.MinOrder, &rt.IsOpen, &rt.Lat, &rt.Lon, &rt.PreviewURL); err != nil {
			c.log.Error("scan restaurant", "err", err)
			writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
			return
		}
		// distance в метрах считаем в коде — на PoC PostGIS не подключаем,
		// фильтр по расстоянию делает клиент или верхний слой
		rt.Distance = haversineMeters(lat, lon, rt.Lat, rt.Lon)
		items = append(items, rt)
	}

	// фильтр по radius на стороне приложения, чтобы не тянуть PostGIS в PoC
	out := items[:0]
	for _, it := range items {
		if it.Distance <= radius {
			out = append(out, it)
		}
	}

	resp := listResp{Total: len(out), Page: page, PerPage: perPage, Items: out}
	body, _ := json.Marshal(resp)

	if c.cfg.cacheEnabled {
		_ = c.rdb.Set(r.Context(), cacheKey, body, c.cfg.cacheTTL).Err()
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.Write(body)
}

type menuItem struct {
	ID          string `json:"menu_item_id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Price       int    `json:"price"`
	Category    string `json:"category"`
	IsAvailable bool   `json:"is_available"`
}

type restaurantCard struct {
	restaurant
	Menu []menuItem `json:"menu"`
}

func (c *catalog) getRestaurant(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if len(id) != 36 {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "bad id")
		return
	}

	cacheKey := ""
	if c.cfg.cacheEnabled {
		cacheKey = "cat:rest:" + id
		if v, err := c.rdb.Get(r.Context(), cacheKey).Bytes(); err == nil {
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("X-Cache", "HIT")
			w.Write(v)
			return
		}
	}

	var rt restaurant
	err := c.db.QueryRow(r.Context(),
		`SELECT id, name, cuisine, rating, eta_minutes, delivery_fee, min_order, is_open, lat, lon, COALESCE(preview_url,'')
		   FROM restaurants WHERE id = $1`, id).
		Scan(&rt.ID, &rt.Name, &rt.Cuisine, &rt.Rating, &rt.ETAMinutes,
			&rt.DeliveryFee, &rt.MinOrder, &rt.IsOpen, &rt.Lat, &rt.Lon, &rt.PreviewURL)
	if err != nil {
		writeProblem(w, http.StatusNotFound, "RESTAURANT_NOT_FOUND", "")
		return
	}

	rows, err := c.db.Query(r.Context(),
		`SELECT id, name, description, price, category, is_available
		   FROM menu_items WHERE restaurant_id = $1 AND is_available = TRUE
		  ORDER BY category, name`, id)
	if err != nil {
		c.log.Error("query menu", "err", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}
	defer rows.Close()

	menu := make([]menuItem, 0, 32)
	for rows.Next() {
		var m menuItem
		if err := rows.Scan(&m.ID, &m.Name, &m.Description, &m.Price, &m.Category, &m.IsAvailable); err != nil {
			c.log.Error("scan menu", "err", err)
			writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
			return
		}
		menu = append(menu, m)
	}

	card := restaurantCard{restaurant: rt, Menu: menu}
	body, _ := json.Marshal(card)

	if c.cfg.cacheEnabled {
		_ = c.rdb.Set(r.Context(), cacheKey, body, c.cfg.cacheTTL).Err()
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.Write(body)
}

// ---------- helpers ----------

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

func parseFloat(s string, def float64) float64 {
	v, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return def
	}
	return v
}

func parseDur(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

func splitCSV(s string) []string {
	if s == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func roundCoord(v float64) string {
	// округление до ~100 м, чтобы соседние точки шерили кэш-ключ
	return strconv.FormatFloat(math.Round(v*1000)/1000, 'f', 3, 64)
}

func haversineMeters(lat1, lon1, lat2, lon2 float64) int {
	const R = 6371000.0
	rad := math.Pi / 180
	dLat := (lat2 - lat1) * rad
	dLon := (lon2 - lon1) * rad
	a := math.Sin(dLat/2)*math.Sin(dLat/2) +
		math.Cos(lat1*rad)*math.Cos(lat2*rad)*math.Sin(dLon/2)*math.Sin(dLon/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return int(R * c)
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
