// Тонкая обёртка над HTTP-логином и WebSocket-каналом.
// Протокол сообщений описан в /PROTOCOL.md в корне репозитория.
//
// В dev-режиме (`npm run dev`) клиент на :5173, сервер на :8080 — разные
// origin, адрес приходится знать заранее. В проде (`npm run build`, сервер
// сам раздаёт dist/) клиент и API — один и тот же origin: подставляем его,
// иначе игрок, зашедший с другого ноутбука по LAN на IP сервера, долбился
// бы в свой собственный localhost:8080 вместо сервера.
const DEFAULT_SERVER = import.meta.env.DEV ? "http://localhost:8080" : window.location.origin;

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
