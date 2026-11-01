import { defineConfig } from "vite";

// Both dev and preview serve the browser and proxy all game traffic to one Go
// process. No client should accidentally connect to a different localhost.
// Go now only speaks HTTPS (self-signed cert, see server/cmd/server/tls.go)
// — secure:false tells Vite's proxy not to reject that cert as untrusted.
function gameProxy() {
  return {
    "/api": { target: "https://127.0.0.1:8080", secure: false },
    "/ws": { target: "wss://127.0.0.1:8080", ws: true, secure: false },
  };
}

export default defineConfig({
  server: { proxy: gameProxy() },
  preview: { proxy: gameProxy() },
  // Two pages: the game and the admin panel (/admin.html), both served by Go
  // from dist/ in production.
  build: {
    rollupOptions: {
      input: { main: "index.html", admin: "admin.html" },
    },
  },
});
