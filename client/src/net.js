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

  async login(name) {
    const res = await fetch(`${this.serverUrl}/api/login`, {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name }),
    });
    if (!res.ok) throw new Error(`Сервер ${this.serverUrl} не выполнил вход (HTTP ${res.status}). Проверь, что Go-сервер запущен.`);
    return res.json();
  }

  connect(token) {
    const wsUrl = this.serverUrl.replace(/^http/, "ws") + `/ws?token=${encodeURIComponent(token)}`;
    this.ws = new WebSocket(wsUrl);

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
        reject(new Error(`Соединение с ${this.serverUrl} закрыто.`));
        this.handlers.disconnect?.();
      };
    });
  }

  send(type, payload = {}) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    this.ws.send(JSON.stringify({ type, ...payload }));
  }
}
