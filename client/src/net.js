// Тонкая обёртка над HTTP-логином и WebSocket-каналом.
// Протокол сообщений описан в /PROTOCOL.md в корне репозитория.
const DEFAULT_SERVER = "http://localhost:8080";

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
    if (!res.ok) throw new Error(`login failed: ${res.status}`);
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
      this.ws.onerror = (err) => reject(err);
    });
  }

  send(type, payload = {}) {
    if (!this.ws || this.ws.readyState !== WebSocket.OPEN) return;
    this.ws.send(JSON.stringify({ type, ...payload }));
  }
}
