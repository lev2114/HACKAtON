// Панель администратора: мониторинг клиентов (ping/FPS и др.), управление
// миром, персонажами-агентами, фактами и глобальными настройками.
// Работает только через /api/admin/* (см. PROTOCOL.md → "Админ-панель").
// Весь пользовательский/серверный текст вставляется через textContent —
// имена игроков и реплики NPC приходят от людей и LLM.

const TOKEN_KEY = "sharolet.admin";
const POLL_MS = 1000;
const HISTORY_LEN = 60;

let token = sessionStorage.getItem(TOKEN_KEY);
let config = null;
let roomNames = {};
let pollTimer = null;
let lastMetrics = null;
const history = new Map(); // playerId -> { fps: [], ping: [] }

const $ = (id) => document.getElementById(id);

// --- helpers ---

function h(tag, props = {}, ...children) {
  const el = document.createElement(tag);
  for (const [key, value] of Object.entries(props)) {
    if (value == null || value === false) continue;
    if (key.startsWith("on")) el.addEventListener(key.slice(2), value);
    else if (key === "class") el.className = value;
    else if (key === "value" || key === "checked" || key === "selected" || key === "disabled") el[key] = value;
    else el.setAttribute(key, value === true ? "" : value);
  }
  for (const child of children.flat()) {
    if (child == null || child === false) continue;
    el.append(child instanceof Node ? child : String(child));
  }
  return el;
}

async function api(method, path, body) {
  const headers = { Authorization: `Bearer ${token}` };
  if (body !== undefined) headers["Content-Type"] = "application/json";
  const res = await fetch(path, { method, headers, body: body === undefined ? undefined : JSON.stringify(body) });
  const data = await res.json().catch(() => null);
  if (res.status === 401) {
    logout("Сессия истекла — войди снова.");
    throw new Error("unauthorized");
  }
  if (!res.ok) throw new Error(data?.error || `HTTP ${res.status}`);
  return data;
}

function status(el, text, ok = true) {
  el.textContent = text;
  el.className = ok ? "ok" : "err";
  if (ok) setTimeout(() => el.textContent === text && (el.textContent = ""), 3000);
}

const fmt = (n, digits = 0) => (Number.isFinite(n) ? n.toFixed(digits) : "—");
const fmtFps = (fps) => fmt(fps, fps < 10 ? 1 : 0);
const pingClass = (ms) => (ms < 80 ? "good" : ms < 200 ? "warn" : "bad");
const fpsClass = (fps) => (fps >= 50 ? "good" : fps >= 30 ? "warn" : "bad");

function duration(sec) {
  sec = Math.max(0, Math.floor(sec));
  const h = Math.floor(sec / 3600), m = Math.floor((sec % 3600) / 60), s = sec % 60;
  return h ? `${h}ч ${m}м` : m ? `${m}м ${s}с` : `${s}с`;
}

function sparkline(values, color, fixedMax) {
  const w = 120, hgt = 26;
  const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
  svg.setAttribute("class", "spark");
  svg.setAttribute("width", w);
  svg.setAttribute("height", hgt);
  if (values.length < 2) return svg;
  const max = fixedMax ?? Math.max(...values, 1);
  const pts = values.map((v, i) => `${(i / (HISTORY_LEN - 1)) * w},${hgt - 2 - (Math.min(v, max) / max) * (hgt - 4)}`);
  const line = document.createElementNS("http://www.w3.org/2000/svg", "polyline");
  line.setAttribute("points", pts.join(" "));
  line.setAttribute("fill", "none");
  line.setAttribute("stroke", color);
  line.setAttribute("stroke-width", "1.5");
  svg.append(line);
  return svg;
}

// --- auth ---

$("login-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = $("login-name").value.trim();
  const password = $("login-password").value;
  $("login-error").textContent = "";
  if (!name || !password) {
    $("login-error").textContent = "Введи имя и пароль";
    return;
  }
  try {
    const res = await fetch("/api/admin/login", {
      method: "POST",
      headers: { "Content-Type": "application/json" },
      body: JSON.stringify({ name, password }),
    });
    const data = await res.json().catch(() => null);
    if (!res.ok) throw new Error(data?.error || `HTTP ${res.status}`);
    token = data.token;
    sessionStorage.setItem(TOKEN_KEY, token);
    start();
  } catch (err) {
    $("login-error").textContent = err.message;
  }
});

function logout(message = "") {
  token = null;
  sessionStorage.removeItem(TOKEN_KEY);
  clearTimeout(pollTimer);
  $("app").hidden = true;
  $("login-screen").hidden = false;
  $("login-screen").style.display = "";
  $("login-error").textContent = message;
}
$("logout").addEventListener("click", () => logout());

async function start() {
  $("login-screen").hidden = true;
  $("login-screen").style.display = "none";
  $("app").hidden = false;
  try {
    await loadConfig();
  } catch (err) {
    if (err.message !== "unauthorized") $("conn-state").textContent = err.message;
    return;
  }
  poll();
}

// --- tabs ---

for (const btn of document.querySelectorAll("#tabs button")) {
  btn.addEventListener("click", () => {
    for (const b of document.querySelectorAll("#tabs button")) b.classList.toggle("active", b === btn);
    for (const s of document.querySelectorAll("main section")) s.classList.toggle("active", s.id === `tab-${btn.dataset.tab}`);
  });
}

// --- config ---

async function loadConfig() {
  config = await api("GET", "/api/admin/config");
  roomNames = Object.fromEntries(config.rooms.map((r) => [r.id, r.name]));
  renderNpcList();
  renderBankSelect();
  fillSettings(config.settings);
}

// --- monitoring ---

async function poll() {
  clearTimeout(pollTimer);
  try {
    lastMetrics = await api("GET", "/api/admin/metrics");
    $("conn-state").textContent = `обновлено ${new Date().toLocaleTimeString()}`;
    $("conn-state").className = "muted";
    renderServer(lastMetrics.server);
    renderClients(lastMetrics.clients, lastMetrics.server.serverTime);
    renderRooms(lastMetrics.server.playersPerRoom);
    renderDoors(lastMetrics.doors);
    renderOllamaInfo(lastMetrics.server);
  } catch (err) {
    if (err.message === "unauthorized") return;
    $("conn-state").textContent = `нет связи с сервером: ${err.message}`;
    $("conn-state").className = "err";
  }
  pollTimer = setTimeout(poll, POLL_MS);
}

function renderServer(s) {
  const llm = s.llm;
  const cards = [
    [s.clients, "клиентов онлайн"],
    [fmtFps(s.avgFps), "средний FPS", s.clients ? fpsClass(s.avgFps) : ""],
    [fmtFps(s.minFps), "минимальный FPS", s.clients ? fpsClass(s.minFps) : ""],
    [fmt(s.avgPingMs, 1), "средний ping, мс", s.clients ? pingClass(s.avgPingMs) : ""],
    [fmt(s.maxPingMs, 1), "максимальный ping, мс", s.clients ? pingClass(s.maxPingMs) : ""],
    [duration(s.uptimeSec), "аптайм сервера"],
    [fmt(s.heapMB, 1), "Go heap, МБ"],
    [s.goroutines, "горутин"],
    [!s.ollamaUrl ? "нет" : s.llmEnabled ? "вкл" : "выкл", `LLM · ${s.model}`],
    [llm.requests, `запросов к LLM (ошибок ${llm.failures})`, llm.failures ? "warn" : ""],
    [llm.requests - llm.failures ? fmt(llm.avgLatencyMs / 1000, 1) : "—", "средний ответ LLM, с"],
    [llm.inFlight, "LLM генерирует сейчас"],
    [llm.fallbacks, "ответов по шаблону (FSM)"],
    [`${s.cleaners}${s.cleanersEnabled ? "" : " ⏸"}`, "уборщиков"],
  ];
  $("server-cards").replaceChildren(
    ...cards.map(([v, k, cls]) => h("div", { class: "card" }, h("div", { class: `v ${cls || ""}` }, v), h("div", { class: "k" }, k)))
  );
}

const clientRows = new Map(); // playerId -> { tr, cells }

function renderClients(clients, serverTime) {
  const tbody = $("clients");
  const now = new Date(serverTime).getTime();
  const seen = new Set();

  for (const c of clients) {
    seen.add(c.playerId);
    const hist = history.get(c.playerId) ?? { fps: [], ping: [] };
    history.set(c.playerId, hist);
    const reported = !!c.stats.lastReport && !c.stats.lastReport.startsWith("0001");
    if (reported) {
      hist.fps.push(c.stats.fps);
      hist.ping.push(c.stats.pingMs);
      if (hist.fps.length > HISTORY_LEN) hist.fps.shift();
      if (hist.ping.length > HISTORY_LEN) hist.ping.shift();
    }

    let row = clientRows.get(c.playerId);
    if (!row) row = createClientRow(c);
    const { cells } = row;
    cells.name.textContent = c.name;
    cells.room.textContent = roomNames[c.room] ?? c.room;
    cells.ping.textContent = reported ? fmt(c.stats.pingMs, 1) : "…";
    cells.ping.className = `num ${reported ? pingClass(c.stats.pingMs) : ""}`;
    cells.pingSpark.replaceChildren(sparkline(hist.ping, "#f0b429"));
    cells.fps.textContent = reported ? fmtFps(c.stats.fps) : "…";
    cells.fps.className = `num ${reported ? fpsClass(c.stats.fps) : ""}`;
    cells.fpsSpark.replaceChildren(sparkline(hist.fps, "#3ccf8e", 75));
    cells.frame.textContent = reported ? fmt(c.stats.frameMs, 1) : "…";
    cells.mem.textContent = c.stats.memoryMB ? fmt(c.stats.memoryMB, 0) : "—";
    cells.draws.textContent = reported ? c.stats.drawCalls : "…";
    cells.tris.textContent = reported ? c.stats.triangles.toLocaleString("ru") : "…";
    const gpu = c.stats.gpu || "";
    cells.gpu.textContent = gpu.replace(/^ANGLE \((.*)\)$/, "$1").replace(/ \(0x[0-9A-F]+\)| Direct3D.*$|, D3D.*$/gi, "") || "…";
    cells.gpu.title = gpu;
    cells.gpu.className = /intel|uhd|iris|swiftshader|llvmpipe|microsoft basic/i.test(gpu) ? "bad" : "";
    const state = [];
    if (c.stats.chatOpen) state.push("в диалоге");
    if (c.stats.paused) state.push("пауза");
    if (c.stats.droppedMessages) state.push(`потеряно сообщений: ${c.stats.droppedMessages}`);
    cells.state.textContent = state.join(", ") || "играет";
    cells.ip.textContent = c.remoteAddr;
    cells.since.textContent = duration((now - new Date(c.connectedAt).getTime()) / 1000);
    if (!row.tr.isConnected) tbody.append(row.tr);
  }

  for (const [id, row] of clientRows) {
    if (seen.has(id)) continue;
    row.tr.remove();
    clientRows.delete(id);
    history.delete(id);
  }
  if (!clients.length) {
    tbody.replaceChildren(h("tr", {}, h("td", { colspan: 15, class: "muted" }, "Никто не подключён")));
  } else {
    for (const row of tbody.querySelectorAll("tr:not([data-player])")) row.remove();
  }
}

function createClientRow(c) {
  const cells = {};
  const td = (key, cls) => (cells[key] = h("td", { class: cls }));
  const roomSelect = h(
    "select",
    {},
    config.rooms.map((r) => h("option", { value: r.id }, r.name))
  );
  const actions = h(
    "td",
    {},
    h(
      "div",
      { class: "actions" },
      roomSelect,
      h("button", {
        class: "secondary",
        title: "Телепортировать в выбранную комнату",
        onclick: async () => {
          try {
            await api("POST", `/api/admin/players/${encodeURIComponent(c.playerId)}/teleport`, { room: roomSelect.value });
          } catch (err) {
            alert(err.message);
          }
        },
      }, "Телепорт"),
      h("button", {
        class: "danger",
        onclick: async () => {
          if (!confirm(`Отключить игрока ${c.name}?`)) return;
          try {
            await api("POST", `/api/admin/players/${encodeURIComponent(c.playerId)}/kick`);
          } catch (err) {
            alert(err.message);
          }
        },
      }, "Кик")
    )
  );
  const tr = h(
    "tr",
    { "data-player": c.playerId },
    td("name"), td("room"), td("ping", "num"), td("pingSpark"), td("fps", "num"), td("fpsSpark"),
    td("frame", "num"), td("mem", "num"), td("draws", "num"), td("tris", "num"),
    td("gpu"), td("state"), td("ip", "muted"), td("since"), actions
  );
  const row = { tr, cells };
  clientRows.set(c.playerId, row);
  return row;
}

// --- world ---

let roomsSignature = "";
function renderRooms(perRoom) {
  const sig = JSON.stringify(perRoom);
  if (sig === roomsSignature) return;
  roomsSignature = sig;
  $("room-cards").replaceChildren(
    ...config.rooms.map((r) =>
      h("div", { class: "card" }, h("div", { class: "v" }, perRoom[r.id] ?? 0), h("div", { class: "k" }, r.name))
    )
  );
}

let doorsSignature = "";
function renderDoors(doors) {
  const sig = JSON.stringify(doors);
  if (sig === doorsSignature) return;
  doorsSignature = sig;
  $("doors").replaceChildren(
    ...doors.map((d) =>
      h(
        "tr",
        {},
        h("td", {}, roomNames[d.a] ?? d.a),
        h("td", {}, roomNames[d.b] ?? d.b),
        h("td", {}, h("span", { class: `pill ${d.open ? "on" : ""}` }, d.open ? "открыта" : "закрыта")),
        h("td", {}, h("button", {
          class: "secondary",
          onclick: async (e) => {
            e.target.disabled = true;
            try {
              await api("POST", "/api/admin/doors", { a: d.a, b: d.b, open: !d.open });
            } catch (err) {
              alert(err.message);
            }
          },
        }, d.open ? "Закрыть" : "Открыть"))
      )
    )
  );
}

$("announce-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const text = $("announce-text").value.trim();
  if (!text) return;
  try {
    const res = await api("POST", "/api/admin/announce", { text });
    $("announce-text").value = "";
    status($("announce-status"), `Отправлено игрокам: ${res.recipients}`);
  } catch (err) {
    status($("announce-status"), err.message, false);
  }
});

// --- agents ---

let selectedNpc = null;

function renderNpcList() {
  $("npc-list").replaceChildren(
    ...config.npcs.map((n) =>
      h(
        "button",
        { class: n.id === selectedNpc ? "active" : "", onclick: () => selectNpc(n.id) },
        `${n.name}${n.overridden ? " ✎" : ""}`,
        h("small", {}, `${n.roomName} · ${modeLabel(n.profile.mode || "llm")}`)
      )
    )
  );
  if (!selectedNpc && config.npcs.length) selectNpc(config.npcs[0].id);
}

const MODES = { llm: "LLM (Ollama)", fsm: "Только шаблоны", silent: "Молчит" };
const modeLabel = (m) => MODES[m] ?? m;

function selectNpc(id) {
  const npc = config.npcs.find((n) => n.id === id);
  if (!npc) return;
  selectedNpc = id;
  for (const [i, b] of [...$("npc-list").children].entries()) b.classList.toggle("active", config.npcs[i].id === id);
  renderNpcEditor(npc);
}

function renderNpcEditor(npc) {
  const p = npc.profile;
  const d = npc.defaults;
  const field = (labelText, el) => h("label", {}, labelText, el);
  const name = h("input", { value: npc.name, maxlength: 100 });
  const title = h("input", { value: npc.title, maxlength: 200 });
  const personality = h("input", { value: npc.personality, maxlength: 1000 });
  const mode = h("select", {}, Object.entries(MODES).map(([v, l]) => h("option", { value: v, selected: (p.mode || "llm") === v }, l)));
  const temperature = h("input", { type: "number", step: "0.1", min: 0, max: 2, placeholder: "по умолчанию модели", value: p.temperature ?? "" });
  const maxTokens = h("input", { type: "number", step: "10", min: 0, max: 2048, placeholder: "без ограничения", value: p.maxTokens || "" });
  const prompt = h("textarea", { rows: 10, placeholder: d.systemPrompt, value: p.systemPrompt ?? "" });
  const greeting = h("textarea", { rows: 2, placeholder: d.greeting, value: p.greeting ?? "" });
  const offtopic = h("textarea", { rows: 2, placeholder: d.offtopic, value: p.offtopic ?? "" });
  const saveStatus = h("span", { class: "muted" });
  const fillDefault = (el, text) => h("button", { type: "button", class: "secondary", onclick: () => (el.value = text) }, "Подставить исходный текст");

  const save = async () => {
    const temp = temperature.value === "" ? null : Number(temperature.value);
    const body = {
      name: name.value,
      title: title.value,
      personality: personality.value,
      profile: {
        systemPrompt: prompt.value.trim(),
        greeting: greeting.value.trim(),
        offtopic: offtopic.value.trim(),
        mode: mode.value,
        maxTokens: Number(maxTokens.value) || 0,
        ...(temp === null ? {} : { temperature: temp }),
      },
    };
    try {
      await api("PUT", `/api/admin/npcs/${encodeURIComponent(npc.id)}`, body);
      await loadConfig();
      selectNpc(npc.id);
      status(saveStatus, "Сохранено — действует со следующей реплики");
    } catch (err) {
      status(saveStatus, err.message, false);
    }
  };
  const reset = async () => {
    if (!confirm(`Вернуть ${npc.base.name} исходный характер и настройки?`)) return;
    try {
      await api("DELETE", `/api/admin/npcs/${encodeURIComponent(npc.id)}`);
      await loadConfig();
      selectNpc(npc.id);
    } catch (err) {
      status(saveStatus, err.message, false);
    }
  };

  const memory = h("div", {}, h("div", { class: "muted" }, "Загрузка…"));
  $("npc-editor").replaceChildren(
    h("div", { class: "box editor" },
      h("div", { class: "muted" }, `id: ${npc.id} · комната: ${npc.roomName} · банк фактов: ${npc.factBank}${npc.overridden ? " · есть правки администратора" : ""}`),
      h("div", { class: "row" }, field("Имя", name), field("Титул", title)),
      field("Характер (используется в общем промпте, если нет своего)", personality),
      h("div", { class: "row" }, field("Режим ответа", mode), field("Температура (0–2)", temperature), field("Макс. длина ответа, токенов", maxTokens)),
      field("Системный промпт (пусто — исходный, показан серым)", prompt),
      h("div", { class: "actions" }, fillDefault(prompt, d.systemPrompt)),
      field("Приветствие (на «Привет!» при открытии диалога)", greeting),
      field("Ответ на оффтоп без LLM", offtopic),
      h("div", { class: "actions" },
        h("button", { onclick: save }, "Сохранить"),
        h("button", { class: "danger", onclick: reset, disabled: !npc.overridden }, "Сбросить к исходному"),
        saveStatus)
    ),
    h("h2", {}, "Память персонажа"),
    npc.sharedMemoryWith?.length
      ? h("p", { class: "muted" }, `Это тот же персонаж, что и в комнате «${npc.sharedMemoryWith.map((id) => config.npcs.find((n) => n.id === id)?.roomName ?? id).join(", ")}»: память диалогов у них общая, сброс действует на обоих. Рассказанные факты у каждого свои.`)
      : "",
    h("div", { class: "box" }, memory)
  );
  loadMemory(npc, memory);
}

async function loadMemory(npc, container) {
  let convs;
  try {
    convs = await api("GET", `/api/admin/npcs/${encodeURIComponent(npc.id)}/memory`);
  } catch (err) {
    container.replaceChildren(h("div", { class: "err" }, err.message));
    return;
  }
  const resetMemory = async (playerId, who) => {
    if (!confirm(`${npc.name} забудет разговоры и рассказанные факты ${who}. Продолжить?`)) return;
    try {
      const q = playerId ? `?player=${encodeURIComponent(playerId)}` : "";
      await api("DELETE", `/api/admin/npcs/${encodeURIComponent(npc.id)}/memory${q}`);
      loadMemory(npc, container);
    } catch (err) {
      alert(err.message);
    }
  };
  const header = h("div", { class: "actions" },
    h("button", { class: "secondary", onclick: () => loadMemory(npc, container) }, "Обновить"),
    h("button", { class: "danger", onclick: () => resetMemory("", "со всеми игроками"), disabled: !convs.length }, "Стереть память обо всех"),
    h("span", { class: "muted" }, "Диалоги хранятся в памяти сервера (последние 40 реплик), рассказанные факты — в SQLite.")
  );
  if (!convs.length) {
    container.replaceChildren(header, h("p", { class: "muted" }, `${npc.name} пока ни с кем не говорил.`));
    return;
  }
  container.replaceChildren(
    header,
    ...convs.map((c) =>
      h("div", { class: "conv" },
        h("div", { class: "actions" },
          h("strong", {}, c.name || c.playerId),
          h("span", { class: "muted" }, `рассказано фактов: ${c.toldFacts}`),
          h("button", { class: "secondary", onclick: () => resetMemory(c.playerId, `с игроком ${c.name || c.playerId}`) }, "Забыть этого игрока")),
        c.lines.length
          ? h("div", { class: "transcript" },
              c.lines.map((l) =>
                h("div", { class: l.role }, `${new Date(l.at).toLocaleTimeString()}${l.npcId && l.npcId !== npc.id ? ` · ${config.npcs.find((n) => n.id === l.npcId)?.roomName ?? l.npcId}` : ""} · ${l.role === "user" ? c.name || "игрок" : npc.name}: `, l.content)))
          : h("div", { class: "muted" }, "Реплик в памяти нет (после перезапуска сервера диалоги не сохраняются).")
      )
    )
  );
}

// --- facts ---

let currentBank = null;
let facts = [];

function renderBankSelect() {
  const select = $("bank-select");
  const users = (bank) => config.npcs.filter((n) => n.factBank === bank).map((n) => n.name).join(", ");
  select.replaceChildren(
    ...config.factBanks.map((b) =>
      h("option", { value: b.id, selected: b.id === currentBank }, `${b.id} (${b.count})${users(b.id) ? ` — ${users(b.id)}` : ""}${b.overridden ? " ✎" : ""}`)
    )
  );
  if (!currentBank && config.factBanks.length) loadBank(config.factBanks[0].id);
}

$("bank-select").addEventListener("change", (e) => loadBank(e.target.value));

async function loadBank(bank) {
  currentBank = bank;
  try {
    facts = await api("GET", `/api/admin/facts/${encodeURIComponent(bank)}`);
    renderFacts();
  } catch (err) {
    status($("facts-status"), err.message, false);
  }
}

function renderFacts() {
  const difficulties = { beginner: "начальный", intermediate: "средний", advanced: "сложный" };
  $("facts").replaceChildren(
    ...facts.map((f, i) =>
      h("div", { class: "fact" },
        h("label", {}, "Тема", h("input", { value: f.topic, oninput: (e) => (f.topic = e.target.value) })),
        h("label", {}, "Сложность",
          h("select", { onchange: (e) => (f.difficulty = e.target.value) },
            Object.entries(difficulties).map(([v, l]) => h("option", { value: v, selected: f.difficulty === v }, l)))),
        h("label", {}, `Текст${f.id ? ` · ${f.id}` : " · новый"}`,
          h("textarea", { rows: 2, value: f.text, oninput: (e) => (f.text = e.target.value) })),
        h("button", { class: "danger", title: "Удалить факт", onclick: () => { facts.splice(i, 1); renderFacts(); } }, "✕")
      )
    ),
    facts.length ? "" : h("p", { class: "muted" }, "В банке нет фактов — персонаж сможет только болтать.")
  );
}

$("fact-add").addEventListener("click", () => {
  facts.push({ id: "", topic: "", difficulty: "beginner", text: "" });
  renderFacts();
  $("facts").lastElementChild?.querySelector("input")?.focus();
});

$("facts-save").addEventListener("click", async () => {
  try {
    facts = await api("PUT", `/api/admin/facts/${encodeURIComponent(currentBank)}`, facts);
    renderFacts();
    await loadConfig();
    status($("facts-status"), "Сохранено");
  } catch (err) {
    status($("facts-status"), err.message, false);
  }
});

$("facts-reset").addEventListener("click", async () => {
  if (!confirm(`Вернуть банк ${currentBank} к исходным фактам из world-data?`)) return;
  try {
    facts = await api("DELETE", `/api/admin/facts/${encodeURIComponent(currentBank)}`);
    renderFacts();
    await loadConfig();
    status($("facts-status"), "Исходные факты восстановлены");
  } catch (err) {
    status($("facts-status"), err.message, false);
  }
});

// --- settings ---

function fillSettings(s) {
  const form = $("settings-form");
  for (const [key, value] of Object.entries(s)) {
    if (form.elements[key]) form.elements[key].value = String(value);
  }
}

function renderOllamaInfo(s) {
  $("ollama-info").textContent = s.ollamaUrl
    ? `Ollama: ${s.ollamaUrl}`
    : "OLLAMA_URL на сервере не задан — LLM недоступна, все персонажи отвечают по шаблонам.";
}

$("settings-form").addEventListener("submit", async (e) => {
  e.preventDefault();
  const f = e.target.elements;
  const body = {
    walkSpeed: Number(f.walkSpeed.value),
    sprintSpeed: Number(f.sprintSpeed.value),
    llmEnabled: f.llmEnabled.value === "true",
    model: f.model.value.trim(),
    cleanersEnabled: f.cleanersEnabled.value === "true",
    cleanerSpeed: Number(f.cleanerSpeed.value),
  };
  try {
    fillSettings(await api("PUT", "/api/admin/settings", body));
    status($("settings-status"), "Сохранено и применено у всех игроков");
  } catch (err) {
    status($("settings-status"), err.message, false);
  }
});

$("settings-reset").addEventListener("click", async () => {
  if (!confirm("Вернуть глобальные настройки к исходным?")) return;
  try {
    fillSettings(await api("DELETE", "/api/admin/settings"));
    status($("settings-status"), "Исходные настройки восстановлены");
  } catch (err) {
    status($("settings-status"), err.message, false);
  }
});

if (token) start();
