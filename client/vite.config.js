import { defineConfig } from "vite";

// Both dev and preview serve the browser and proxy all game traffic to one Go
// process. No client should accidentally connect to a different localhost.
function gameProxy() {
  return {
    "/api": { target: "http://127.0.0.1:8080" },
    "/ws": { target: "ws://127.0.0.1:8080", ws: true },
  };
}

export default defineConfig({
  server: { proxy: gameProxy() },
  preview: { proxy: gameProxy() },
});
