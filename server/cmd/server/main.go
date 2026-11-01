// Шаролёт server: serves the login + WebSocket API, and (once built)
// the client's static files, so the whole game can run from one process
// with no internet access during the offline half of the demo.
package main

import (
	"context"
	"log"
	"net/http"
	"os"

	"server/internal/ai"
	"server/internal/httpapi"
	"server/internal/hub"
	"server/internal/store"
	"server/internal/world"
)

func getenv(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	worldDir := getenv("WORLD_DATA_DIR", "../world-data")
	dbPath := getenv("DB_PATH", "./station.db")
	addr := getenv("ADDR", ":8080")
	clientDist := getenv("CLIENT_DIST_DIR", "../client/dist")
	certDir := getenv("CERT_DIR", ".")

	// Пусто = ИИ сразу идёт в FSM-фоллбэк, без похода к Ollama.
	ollamaURL := os.Getenv("OLLAMA_URL")
	model := getenv("OLLAMA_MODEL", "qwen2.5:7b")

	w, err := world.Load(worldDir)
	if err != nil {
		log.Fatalf("не удалось загрузить world-data из %s: %v", worldDir, err)
	}

	st, err := store.Open(dbPath)
	if err != nil {
		log.Fatalf("не удалось открыть БД %s: %v", dbPath, err)
	}

	h := hub.New()
	responder := ai.New(ollamaURL, model)
	srv := httpapi.New(w, st, h, responder)
	go srv.RunFacility(context.Background())

	mux := http.NewServeMux()
	mux.HandleFunc("/api/register", srv.Register)
	mux.HandleFunc("/api/login", srv.Login)
	mux.HandleFunc("/ws", srv.WS)
	mux.Handle("/", http.FileServer(http.Dir(clientDist)))

	certFile, keyFile, err := ensureTLSCert(certDir)
	if err != nil {
		log.Fatalf("не удалось подготовить TLS-сертификат: %v", err)
	}

	log.Printf("Шаролёт слушает на https://%s (world=%s, ollama=%q, model=%q)", addr, worldDir, ollamaURL, model)
	// No CORS headers: Vite's dev/preview proxy forwards /api and /ws to Go
	// server-side (vite.config.js), so the browser only ever talks to the
	// page's own origin — there's no legitimate cross-origin browser request
	// to this server to allow for. A wildcard Access-Control-Allow-Origin
	// here would just be unused attack surface.
	if err := http.ListenAndServeTLS(addr, certFile, keyFile, mux); err != nil {
		log.Fatal(err)
	}
}
