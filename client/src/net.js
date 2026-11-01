// Тонкая обёртка над HTTP-логином и WebSocket-каналом.
// Протокол сообщений описан в /PROTOCOL.md в корне репозитория.
//
// Vite dev/preview proxy /api and /ws to Go; production is served by Go itself.
// In all modes, the browser uses the same origin for the page and game traffic.
const DEFAULT_SERVER = window.location.origin;

export class Net {
  constructor(serverUrl = DEFAULT_SERVER) {
    this.serverUrl = serverUrl;
    this.ws = null;
    this.handlers = {};
  }

  on(type, handler) {
    this.handlers[type] = handler;
  }

  async register(name, password) {
    return this.#auth("register", name, password);
  }

  async login(name, password) {
    return this.#auth("login", name, password);
  }

  async #auth(endpoint, name, password) {
    const res = await fetch(`${this.serverUrl}/api/${endpoint}`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, password }),
    });
    if (!res.ok) {
      const body = await res.json().catch(() => null);
      throw new Error(body?.error || `Сервер ${this.serverUrl} ответил HTTP ${res.status}.`);
    }
    return res.json();
  }

  connect(token) {
    const wsUrl = this.serverUrl.replace(/^http/, "ws") + `/ws?token=${encodeURIComponent(token)}`;
    this.ws = new WebSocket(wsUrl);
    this.intentionalClose = false;

    this.ws.onmessage = (event) => {
      let msg;
      try {
        msg = JSON.parse(event.data);
      } catch (err) {
        console.error("net: failed to parse server message:", err);
        return;
      }
      const handler = this.handlers[msg.type];
      if (handler) handler(msg);
    };

    return new Promise((resolve, reject) => {
      this.ws.onopen = () => resolve();
      this.ws.onerror = () => reject(new Error(`Не удалось открыть WebSocket: ${this.serverUrl}. Проверь Go-сервер.`));
      this.ws.onclose = () => {
        // A deliberate disconnect() (logout) closes the socket itself —
        // that's not the "lost connection" case the disconnect handler
        // communicates to the player, so it's suppressed here.
        if (this.intentionalClose) return;
        reject(new Error(`Соединение с ${this.serverUrl} закрыто.`));
        this.handlers.disconnect?.();
      };
    });
  }

  // Logout: closes the socket without triggering the "lost connection" UI.
  disconnect() {
    this.intentionalClose = true;
    this.ws?.close();
    this.ws = null;
  }

  send(type, payload = {}) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    this.ws.send(JSON.stringify({ type, ...payload }));
  }
}
