// Шаролёт server: serves the login + WebSocket API, and (once built)
// the client's static files, so the whole game can run from one process
// with no internet access during the offline half of the demo.
package main

import (
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

	mux := http.NewServeMux()
	mux.HandleFunc("/api/login", srv.Login)
	mux.HandleFunc("/ws", srv.WS)
	mux.Handle("/", http.FileServer(http.Dir(clientDist)))

	log.Printf("Шаролёт слушает на %s (world=%s, ollama=%q, model=%q)", addr, worldDir, ollamaURL, model)
	if err := http.ListenAndServe(addr, withCORS(mux)); err != nil {
		log.Fatal(err)
	}
}

// withCORS lets the Vite dev server (a different port) talk to this API
// during client development, while production runs on a single origin.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}
