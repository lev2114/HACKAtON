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

  onRoomState(handler) {
    this.on("room_state", (message) => handler(message.state));
  }

  setDoor(room, door, open) {
    this.send("door_set", { room, door, open });
  }

  setNpcConversation(npcId, active) {
    this.send("npc_conversation", { npcId, active });
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
    const socket = new WebSocket(wsUrl);
    this.ws = socket;
    this.intentionalClose = false;
    return new Promise((resolve, reject) => {
      let welcomed = false;
      const fail = (message) => {
        clearTimeout(timeout);
        reject(new Error(message));
        socket.close();
      };
      const timeout = setTimeout(() => fail("Сервер не прислал комнату. Перезапусти Go-сервер и попробуй снова."), 15000);
      socket.onmessage = (event) => {
        if (this.ws !== socket) return;
        let msg;
        try { msg = JSON.parse(event.data); }
        catch (error) { console.error("net: invalid message", error); return; }
        if (!welcomed && msg.type === "error") { fail(msg.message || "Не удалось войти на станцию."); return; }
        if (msg.type === "welcome") {
          if (!msg.chunk?.room) { fail("Сохранённая комната недоступна. Перезапусти обновлённый Go-сервер и войди снова."); return; }
          welcomed = true;
          clearTimeout(timeout);
        }
        const handler = this.handlers[msg.type];
        if (handler) handler(msg);
        if (msg.type === "welcome") resolve();
      };
      socket.onerror = () => fail(`Не удалось открыть WebSocket: ${this.serverUrl}. Проверь Go-сервер.`);
      socket.onclose = () => {
        clearTimeout(timeout);
        if (this.ws !== socket) return;
        // A deliberate disconnect() (logout) closes the socket itself —
        // that's not the "lost connection" case the disconnect handler
        // communicates to the player, so it's suppressed here.
        if (this.intentionalClose) return;
        reject(new Error(`Соединение с ${this.serverUrl} закрыто.`));
        if (welcomed) this.handlers.disconnect?.();
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
