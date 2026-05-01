package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io"
	"log"
	mathrand "math/rand/v2"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"
)

// PSP mock: имитирует внешний платёжный провайдер.
// Поведение настраивается переменными окружения, чтобы k6-сценарии могли проверить
// и happy path, и реакцию circuit breaker'а на деградацию PSP.
//
//   PSP_LATENCY_MS    базовая задержка
//   PSP_JITTER_MS     добавка к задержке (равномерная)
//   PSP_ERROR_RATE    доля 5xx (0.0..1.0)
//   PSP_TIMEOUT_RATE  доля «зависаний» дольше клиентского таймаута

var (
	mu      sync.Mutex
	intents = map[string]string{}
)

func main() {
	addr := getenv("HTTP_ADDR", ":9090")
	latency := time.Duration(parseInt(getenv("PSP_LATENCY_MS", "200"))) * time.Millisecond
	jitter := time.Duration(parseInt(getenv("PSP_JITTER_MS", "150"))) * time.Millisecond
	errorRate := parseFloat(getenv("PSP_ERROR_RATE", "0.0"))
	timeoutRate := parseFloat(getenv("PSP_TIMEOUT_RATE", "0.0"))

	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })

	mux.HandleFunc("POST /v1/intents", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		time.Sleep(latency + time.Duration(mathrand.Int64N(int64(jitter+1))))
		if mathrand.Float64() < timeoutRate {
			time.Sleep(5 * time.Second)
		}
		if mathrand.Float64() < errorRate {
			http.Error(w, `{"error":"psp_unavailable"}`, http.StatusBadGateway)
			return
		}
		intentID := "intent_" + randHex(12)
		mu.Lock()
		intents[intentID] = "processing"
		mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]string{
			"intent_id":    intentID,
			"redirect_url": "https://psp.example.com/pay/" + intentID,
		})
	})

	mux.HandleFunc("GET /v1/intents/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		mu.Lock()
		st, ok := intents[id]
		mu.Unlock()
		if !ok {
			http.Error(w, "not_found", http.StatusNotFound)
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"intent_id": id, "status": st})
	})

	// helper для k6 / ручного теста: пометить intent как succeeded/failed
	mux.HandleFunc("POST /v1/intents/{id}/complete", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		status := r.URL.Query().Get("status")
		if status == "" {
			status = "succeeded"
		}
		mu.Lock()
		intents[id] = status
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	})

	log.Printf("psp-mock up addr=%s latency=%s jitter=%s err_rate=%.2f timeout_rate=%.2f",
		addr, latency, jitter, errorRate, timeoutRate)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 3 * time.Second,
	}
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func getenv(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseInt(s string) int {
	v, _ := strconv.Atoi(s)
	return v
}

func parseFloat(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b) // crypto/rand для уникального intent_id
	return hex.EncodeToString(b)
}
