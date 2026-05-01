package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/segmentio/kafka-go"
	"github.com/sony/gobreaker"
)

type config struct {
	httpAddr     string
	pgDSN        string
	kafkaBrokers string
	pspURL       string
	pgMaxConns   int32
	outboxBatch  int
	outboxPoll   time.Duration
}

func loadConfig() config {
	return config{
		httpAddr:     getenv("HTTP_ADDR", ":8083"),
		pgDSN:        getenv("PG_DSN", "postgres://food:food@postgres:5432/fooddelivery?sslmode=disable"),
		kafkaBrokers: getenv("KAFKA_BROKERS", "redpanda:9092"),
		pspURL:       getenv("PSP_URL", "http://psp-mock:9090"),
		pgMaxConns:   int32(parseInt(getenv("PG_MAX_CONNS", "4"), 4)),
		outboxBatch:  parseInt(getenv("OUTBOX_BATCH", "16"), 16),
		outboxPoll:   parseDur(getenv("OUTBOX_POLL", "1s"), time.Second),
	}
}

type paymentSvc struct {
	db    *pgxpool.Pool
	cfg   config
	log   *slog.Logger
	cb    *gobreaker.CircuitBreaker
	http  *http.Client
	kw    *kafka.Writer
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

	cb := gobreaker.NewCircuitBreaker(gobreaker.Settings{
		Name:        "psp",
		MaxRequests: 3,
		Interval:    30 * time.Second,
		Timeout:     60 * time.Second, // OPEN -> HALF_OPEN через 60s
		ReadyToTrip: func(counts gobreaker.Counts) bool {
			// требуем минимум 10 обращений и >50% ошибок — порог из ADR-003
			return counts.Requests >= 10 && float64(counts.TotalFailures)/float64(counts.Requests) >= 0.5
		},
		OnStateChange: func(name string, from, to gobreaker.State) {
			log.Warn("breaker state", "name", name, "from", from.String(), "to", to.String())
		},
	})

	kw := &kafka.Writer{
		Addr:         kafka.TCP(strings.Split(cfg.kafkaBrokers, ",")...),
		Topic:        "orders.events",
		Balancer:     &kafka.Hash{},
		BatchTimeout: 50 * time.Millisecond,
		BatchSize:    32,
		RequiredAcks: kafka.RequireAll,
		Async:        false,
	}
	defer kw.Close()

	s := &paymentSvc{
		db:   pool,
		cfg:  cfg,
		log:  log,
		cb:   cb,
		http: &http.Client{Timeout: 2500 * time.Millisecond},
		kw:   kw,
	}

	// фоновые воркеры: outbox relay + reconciliation
	go s.runOutboxRelay(ctx)
	go s.runReconciliation(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /api/v1/orders/{id}/payment", s.initiatePayment)
	mux.HandleFunc("POST /webhooks/psp", s.webhookPSP)

	srv := &http.Server{
		Addr:              cfg.httpAddr,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
		WriteTimeout:      8 * time.Second,
		IdleTimeout:       60 * time.Second,
	}
	log.Info("payment up", "addr", cfg.httpAddr, "psp", cfg.pspURL, "kafka", cfg.kafkaBrokers)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("listen", "err", err)
		os.Exit(1)
	}
}

func (s *paymentSvc) ready(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 800*time.Millisecond)
	defer cancel()
	if err := s.db.Ping(ctx); err != nil {
		http.Error(w, "db not ready", http.StatusServiceUnavailable)
		return
	}
	w.Write([]byte("ok"))
}

// ---------- POST /api/v1/orders/{id}/payment ----------

type initiateReq struct {
	PaymentMethod string `json:"payment_method"`
	ReturnURL     string `json:"return_url,omitempty"`
}

type initiateResp struct {
	PaymentID   string `json:"payment_id"`
	OrderID     string `json:"order_id"`
	Status      string `json:"status"`
	RedirectURL string `json:"redirect_url,omitempty"`
	Amount      int    `json:"amount"`
	Currency    string `json:"currency"`
}

func (s *paymentSvc) initiatePayment(w http.ResponseWriter, r *http.Request) {
	orderID := r.PathValue("id")
	if _, err := uuid.Parse(orderID); err != nil {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "")
		return
	}

	idemKey := r.Header.Get("Idempotency-Key")
	if _, err := uuid.Parse(idemKey); err != nil {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "Idempotency-Key должен быть UUID")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 8*1024))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "")
		return
	}
	var req initiateReq
	if err := json.Unmarshal(body, &req); err != nil || req.PaymentMethod == "" {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "")
		return
	}

	// 1) идемпотентность по uniq key — если уже есть запись, отдаём её состояние
	var existingID, existingStatus string
	var amount int
	err = s.db.QueryRow(r.Context(),
		`SELECT id, status, amount FROM payments WHERE idempotency_key = $1`, idemKey).
		Scan(&existingID, &existingStatus, &amount)
	if err == nil {
		writeJSON(w, http.StatusOK, initiateResp{
			PaymentID: existingID, OrderID: orderID, Status: existingStatus,
			Amount: amount, Currency: "RUB",
		})
		return
	}

	// 2) проверим заказ и его сумму
	var orderStatus string
	err = s.db.QueryRow(r.Context(),
		`SELECT status, total_amount FROM orders WHERE id = $1`, orderID).
		Scan(&orderStatus, &amount)
	if err != nil {
		writeProblem(w, http.StatusNotFound, "ORDER_NOT_FOUND", "")
		return
	}
	if orderStatus != "awaiting_payment" {
		writeProblem(w, http.StatusConflict, "INVALID_ORDER_STATE", orderStatus)
		return
	}

	paymentID := uuid.New()

	// 3) создаём payments=processing
	_, err = s.db.Exec(r.Context(),
		`INSERT INTO payments (id, order_id, status, amount, idempotency_key)
		 VALUES ($1, $2, 'processing', $3, $4)`,
		paymentID, orderID, amount, idemKey)
	if err != nil {
		s.log.Error("insert payment", "err", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}

	// 4) вызов PSP через circuit breaker
	res, err := s.cb.Execute(func() (any, error) {
		return s.callPSP(r.Context(), paymentID.String(), orderID, amount)
	})
	if err != nil {
		// CB OPEN или таймаут PSP — делаем pending_confirmation, не падаем
		if errors.Is(err, gobreaker.ErrOpenState) {
			s.markPending(r.Context(), paymentID, "circuit_breaker_open")
			w.Header().Set("Retry-After", "30")
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"payment_id": paymentID.String(),
				"order_id":   orderID,
				"status":     "payment_provider_unavailable",
			})
			return
		}
		s.markPending(r.Context(), paymentID, err.Error())
		writeJSON(w, http.StatusAccepted, initiateResp{
			PaymentID: paymentID.String(), OrderID: orderID,
			Status: "payment_pending_confirmation", Amount: amount, Currency: "RUB",
		})
		return
	}

	pspResp := res.(*pspResponse)

	// 5) успешный response от PSP — обновляем payments + orders + outbox в одной транзакции
	tx, err := s.db.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}
	defer tx.Rollback(context.Background())

	_, err = tx.Exec(r.Context(),
		`UPDATE payments SET psp_payment_id = $2, status = 'processing' WHERE id = $1`,
		paymentID, pspResp.IntentID)
	if err != nil {
		s.log.Error("update payment", "err", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}
	if err := tx.Commit(r.Context()); err != nil {
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}

	writeJSON(w, http.StatusOK, initiateResp{
		PaymentID:   paymentID.String(),
		OrderID:     orderID,
		Status:      "processing",
		RedirectURL: pspResp.RedirectURL,
		Amount:      amount,
		Currency:    "RUB",
	})
}

func (s *paymentSvc) markPending(ctx context.Context, id uuid.UUID, reason string) {
	_, err := s.db.Exec(ctx,
		`UPDATE payments SET status = 'pending_confirmation', failure_reason = $2 WHERE id = $1`,
		id, reason)
	if err != nil {
		s.log.Error("mark pending", "err", err)
	}
}

// ---------- PSP client ----------

type pspIntentReq struct {
	PaymentID string `json:"payment_id"`
	OrderID   string `json:"order_id"`
	Amount    int    `json:"amount"`
}

type pspResponse struct {
	IntentID    string `json:"intent_id"`
	RedirectURL string `json:"redirect_url"`
}

func (s *paymentSvc) callPSP(ctx context.Context, paymentID, orderID string, amount int) (*pspResponse, error) {
	body, _ := json.Marshal(pspIntentReq{PaymentID: paymentID, OrderID: orderID, Amount: amount})
	ctx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", s.cfg.pspURL+"/v1/intents", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return nil, fmt.Errorf("psp status %d: %s", resp.StatusCode, string(respBody))
	}
	var out pspResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, err
	}
	return &out, nil
}

// ---------- POST /webhooks/psp ----------

type webhookReq struct {
	IntentID string `json:"intent_id"`
	Status   string `json:"status"` // succeeded / failed
	Reason   string `json:"reason,omitempty"`
}

func (s *paymentSvc) webhookPSP(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(io.LimitReader(r.Body, 8*1024))
	if err != nil {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "")
		return
	}
	var hook webhookReq
	if err := json.Unmarshal(body, &hook); err != nil || hook.IntentID == "" {
		writeProblem(w, http.StatusBadRequest, "VALIDATION_ERROR", "")
		return
	}

	// идемпотентный обработчик: дубликат webhook'а — 200 без изменений
	tx, err := s.db.BeginTx(r.Context(), pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}
	defer tx.Rollback(context.Background())

	tag, err := tx.Exec(r.Context(),
		`INSERT INTO processed_webhooks (psp_payment_id, event_type) VALUES ($1, $2)
		 ON CONFLICT DO NOTHING`, hook.IntentID, "payment."+hook.Status)
	if err != nil {
		s.log.Error("insert webhook idem", "err", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}
	if tag.RowsAffected() == 0 {
		// дубликат
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("duplicate"))
		return
	}

	// ADR-003: SELECT ... FOR UPDATE по psp_payment_id
	var paymentID uuid.UUID
	var orderID uuid.UUID
	err = tx.QueryRow(r.Context(),
		`SELECT id, order_id FROM payments WHERE psp_payment_id = $1 FOR UPDATE`, hook.IntentID).
		Scan(&paymentID, &orderID)
	if err != nil {
		writeProblem(w, http.StatusNotFound, "PAYMENT_NOT_FOUND", "")
		return
	}

	now := time.Now().UTC()
	if hook.Status == "succeeded" {
		_, err = tx.Exec(r.Context(),
			`UPDATE payments SET status='succeeded', confirmed_at=$2 WHERE id=$1`, paymentID, now)
		if err == nil {
			_, err = tx.Exec(r.Context(),
				`UPDATE orders SET status='paid', updated_at=$2 WHERE id=$1`, orderID, now)
		}
	} else {
		_, err = tx.Exec(r.Context(),
			`UPDATE payments SET status='failed', failure_reason=$2, confirmed_at=$3 WHERE id=$1`,
			paymentID, hook.Reason, now)
		if err == nil {
			_, err = tx.Exec(r.Context(),
				`UPDATE orders SET status='cancelled', updated_at=$2 WHERE id=$1`, orderID, now)
		}
	}
	if err != nil {
		s.log.Error("update on webhook", "err", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}

	payload, _ := json.Marshal(map[string]any{
		"payment_id": paymentID.String(),
		"order_id":   orderID.String(),
		"status":     hook.Status,
	})
	_, err = tx.Exec(r.Context(),
		`INSERT INTO outbox_events (aggregate_type, aggregate_id, event_type, payload)
		 VALUES ('payment', $1, $2, $3)`, paymentID, "payment."+hook.Status, payload)
	if err != nil {
		s.log.Error("insert outbox", "err", err)
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}

	if err := tx.Commit(r.Context()); err != nil {
		writeProblem(w, http.StatusInternalServerError, "INTERNAL_ERROR", "")
		return
	}
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

// ---------- outbox relay ----------

func (s *paymentSvc) runOutboxRelay(ctx context.Context) {
	ticker := time.NewTicker(s.cfg.outboxPoll)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if n := s.publishBatch(ctx); n > 0 {
				s.log.Debug("outbox published", "n", n)
			}
		}
	}
}

func (s *paymentSvc) publishBatch(ctx context.Context) int {
	rows, err := s.db.Query(ctx,
		`SELECT id, aggregate_type, aggregate_id, event_type, payload::text
		   FROM outbox_events
		  WHERE published_at IS NULL
		  ORDER BY id
		  LIMIT $1
		  FOR UPDATE SKIP LOCKED`, s.cfg.outboxBatch)
	if err != nil {
		// SKIP LOCKED работает только в транзакции — переключим
		return s.publishBatchTx(ctx)
	}
	type row struct {
		id        int64
		aggType   string
		aggID     string
		eventType string
		payload   string
	}
	var batch []row
	for rows.Next() {
		var rw row
		if err := rows.Scan(&rw.id, &rw.aggType, &rw.aggID, &rw.eventType, &rw.payload); err != nil {
			rows.Close()
			return 0
		}
		batch = append(batch, rw)
	}
	rows.Close()
	if len(batch) == 0 {
		return 0
	}

	msgs := make([]kafka.Message, 0, len(batch))
	for _, rw := range batch {
		msgs = append(msgs, kafka.Message{
			Key:   []byte(rw.aggID),
			Value: []byte(rw.payload),
			Headers: []kafka.Header{
				{Key: "event_type", Value: []byte(rw.eventType)},
				{Key: "aggregate_type", Value: []byte(rw.aggType)},
			},
		})
	}
	if err := s.kw.WriteMessages(ctx, msgs...); err != nil {
		s.log.Error("kafka write", "err", err)
		return 0
	}
	ids := make([]int64, 0, len(batch))
	for _, rw := range batch {
		ids = append(ids, rw.id)
	}
	_, err = s.db.Exec(ctx, `UPDATE outbox_events SET published_at = now() WHERE id = ANY($1)`, ids)
	if err != nil {
		s.log.Error("mark published", "err", err)
	}
	return len(batch)
}

func (s *paymentSvc) publishBatchTx(ctx context.Context) int {
	tx, err := s.db.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return 0
	}
	defer tx.Rollback(context.Background())
	rows, err := tx.Query(ctx,
		`SELECT id, aggregate_id, event_type, payload::text
		   FROM outbox_events
		  WHERE published_at IS NULL
		  ORDER BY id
		  LIMIT $1
		  FOR UPDATE SKIP LOCKED`, s.cfg.outboxBatch)
	if err != nil {
		return 0
	}
	type row struct {
		id        int64
		aggID     string
		eventType string
		payload   string
	}
	var batch []row
	for rows.Next() {
		var rw row
		if err := rows.Scan(&rw.id, &rw.aggID, &rw.eventType, &rw.payload); err != nil {
			rows.Close()
			return 0
		}
		batch = append(batch, rw)
	}
	rows.Close()
	if len(batch) == 0 {
		return 0
	}
	msgs := make([]kafka.Message, 0, len(batch))
	for _, rw := range batch {
		msgs = append(msgs, kafka.Message{
			Key:   []byte(rw.aggID),
			Value: []byte(rw.payload),
			Headers: []kafka.Header{{Key: "event_type", Value: []byte(rw.eventType)}},
		})
	}
	if err := s.kw.WriteMessages(ctx, msgs...); err != nil {
		s.log.Error("kafka write", "err", err)
		return 0
	}
	ids := make([]int64, 0, len(batch))
	for _, rw := range batch {
		ids = append(ids, rw.id)
	}
	if _, err := tx.Exec(ctx, `UPDATE outbox_events SET published_at = now() WHERE id = ANY($1)`, ids); err != nil {
		s.log.Error("mark published", "err", err)
		return 0
	}
	if err := tx.Commit(ctx); err != nil {
		return 0
	}
	return len(batch)
}

// ---------- reconciliation ----------

func (s *paymentSvc) runReconciliation(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.reconcileOnce(ctx)
		}
	}
}

func (s *paymentSvc) reconcileOnce(ctx context.Context) {
	// зависшие в pending_confirmation/processing старше 30 секунд — спрашиваем PSP
	rows, err := s.db.Query(ctx,
		`SELECT id, psp_payment_id FROM payments
		  WHERE status IN ('processing','pending_confirmation')
		    AND psp_payment_id IS NOT NULL
		    AND created_at < now() - interval '30 seconds'
		  LIMIT 32`)
	if err != nil {
		return
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var pspID string
		if err := rows.Scan(&id, &pspID); err != nil {
			continue
		}
		// best-effort lookup
		req, _ := http.NewRequestWithContext(ctx, "GET", s.cfg.pspURL+"/v1/intents/"+pspID, nil)
		resp, err := s.http.Do(req)
		if err != nil {
			continue
		}
		var pr struct {
			Status string `json:"status"`
		}
		_ = json.NewDecoder(resp.Body).Decode(&pr)
		resp.Body.Close()
		if pr.Status == "succeeded" || pr.Status == "failed" {
			// эмулируем тот же путь, что и webhook
			body, _ := json.Marshal(webhookReq{IntentID: pspID, Status: pr.Status})
			r, _ := http.NewRequestWithContext(ctx, "POST", "http://127.0.0.1"+s.cfg.httpAddr+"/webhooks/psp", bytes.NewReader(body))
			r.Header.Set("Content-Type", "application/json")
			c := &http.Client{Timeout: 2 * time.Second}
			rr, err := c.Do(r)
			if err == nil {
				rr.Body.Close()
			}
		}
	}
}

// ---------- helpers ----------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
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

func parseDur(s string, def time.Duration) time.Duration {
	d, err := time.ParseDuration(s)
	if err != nil {
		return def
	}
	return d
}

func must(err error, log *slog.Logger, msg string) {
	if err != nil {
		log.Error(msg, "err", err)
		os.Exit(1)
	}
}
