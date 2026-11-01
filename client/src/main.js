import * as THREE from "three";
import { Net } from "./net.js";
import { buildRoom, destroyRoom } from "./world.js";
import { createCosmicSky } from "./sky.js";
import { createRemotePlayer } from "./players.js";
import { CORRIDOR_DEPTH } from "./doors.js";
import { createApple } from "./items.js";

// Рисуем приглушённый звёздный фон для экрана входа один раз на canvas и
// подставляем как CSS-фон — не трогает игровую сцену/рендер-луп (тот
// стартует только после успешного логина).
function paintLoginStarfield(el) {
  const canvas = document.createElement("canvas");
  canvas.width = 900;
  canvas.height = 900;
  const ctx = canvas.getContext("2d");

  const grad = ctx.createRadialGradient(280, 200, 0, 280, 200, 800);
  grad.addColorStop(0, "#140b26");
  grad.addColorStop(0.5, "#08070f");
  grad.addColorStop(1, "#030308");
  ctx.fillStyle = grad;
  ctx.fillRect(0, 0, canvas.width, canvas.height);

  for (let i = 0; i < 260; i++) {
    const x = Math.random() * canvas.width;
    const y = Math.random() * canvas.height;
    const r = Math.random() < 0.85 ? Math.random() * 0.7 + 0.3 : Math.random() * 1.3 + 0.8;
    const alpha = Math.random() * 0.5 + 0.25;
    ctx.beginPath();
    ctx.arc(x, y, r, 0, Math.PI * 2);
    ctx.fillStyle = `rgba(255,255,255,${alpha})`;
    ctx.fill();
  }

  el.style.backgroundImage = `url(${canvas.toDataURL()})`;
  el.style.backgroundSize = "cover";
}

const net = new Net();

const scene = new THREE.Scene();
// Transparent world backdrop: the persistent sky is drawn underneath it.
const cosmicSky = createCosmicSky();

const camera = new THREE.PerspectiveCamera(70, window.innerWidth / window.innerHeight, 0.1, 500);
camera.position.set(0, 1.6, 5);
camera.rotation.order = "YXZ";
scene.add(camera);
const carriedApple = createApple();
carriedApple.position.set(0.23, -0.27, -0.48);
carriedApple.scale.setScalar(0.7);
carriedApple.traverse((node) => { if (node.isMesh) { node.material.depthTest = false; node.renderOrder = 100; } });
carriedApple.visible = false;
camera.add(carriedApple);

// На ноутбуках с двумя видеокартами браузер по умолчанию берёт встроенную:
// на Intel UHD вестибюль даёт ~8 FPS, на GTX 1660 Ti — ~200. Это только
// подсказка браузеру (Windows может её игнорировать) — надёжно помогает
// "Высокая производительность" для браузера в настройках графики Windows.
const renderer = new THREE.WebGLRenderer({ antialias: true, powerPreference: "high-performance" });

// Имя видеокарты для админки: сразу видно, кто играет на встроенной.
const gpuName = (() => {
  const gl = renderer.getContext();
  const ext = gl.getExtension("WEBGL_debug_renderer_info");
  return String(ext ? gl.getParameter(ext.UNMASKED_RENDERER_WEBGL) : gl.getParameter(gl.RENDERER)).slice(0, 160);
})();
renderer.setSize(window.innerWidth, window.innerHeight);
document.body.appendChild(renderer.domElement);

// Только для отладки (devtools/автотесты) — не используется игровым кодом.
window.__sharolet = { camera, scene, renderer };

scene.add(new THREE.HemisphereLight(0xffffff, 0x222244, 1.2));
const sun = new THREE.DirectionalLight(0xffffff, 0.6);
sun.position.set(5, 10, 5);
scene.add(sun);

window.addEventListener("resize", () => {
  camera.aspect = window.innerWidth / window.innerHeight;
  camera.updateProjectionMatrix();
  renderer.setSize(window.innerWidth, window.innerHeight);
});

const players = new Map(); // playerId -> astronaut Group (network eye position)
window.__sharolet.players = players; // для отладки/автотестов, см. строку 18
let currentRoomBuilt = null;
let roomCleanup = Promise.resolve();
let roomVersion = 0;
let facilityRoom = null;
let facilityState = null;

net.onRoomState((state) => {
  if (!state || state.room !== facilityRoom || (facilityState && state.revision < facilityState.revision)) return;
  facilityState = state;
  currentRoomBuilt?.applyState(state);
});
let myId = null;
let myName = null;
let disconnected = false;
let running = false; // тик идёт только пока мы реально в игре (не на логине/паузе)
let paused = false;
let myWorldState = { apples: 0, suitColor: "#ffffff", revision: -1 };
let noticeUntil = 0;

function itemNotice(text) {
  document.getElementById("item-notice").textContent = text;
  noticeUntil = performance.now() + 4500;
}

function applyPlayerWorldState(state) {
  if (!state) return;
  if (state.playerId === myId) {
    if (state.revision < myWorldState.revision) return;
    myWorldState = state;
    carriedApple.visible = state.apples > 0;
    document.getElementById("suit-swatch").style.background = state.suitColor;
    const names = { "#ffffff":"белый", "#ef5350":"красный", "#ff9800":"оранжевый", "#ffdf55":"жёлтый", "#66bb6a":"зелёный", "#4dd0e1":"голубой", "#4f83ff":"синий", "#ab68dc":"фиолетовый", "#f48fb1":"розовый", "#505766":"чёрный" };
    document.getElementById("suit-color-name").textContent = names[state.suitColor] || state.suitColor;
    document.getElementById("apple-count").textContent = `Яблоки: ${state.apples} · ${state.apples ? "F — съесть" : "попроси Совунью"}`;
  } else {
    players.get(state.playerId)?.userData.avatar.setWorldState(state);
  }
}

net.onPlayerState(applyPlayerWorldState);
net.onNpcAction((message) => {
  if (message.room === currentRoomBuilt?.room.id) currentRoomBuilt.actions.event(message);
  if (message.playerId === myId) itemNotice(message.action === "give_apple" ? "Совунья дала тебе яблоко" : "Нюша перекрасила скафандр");
});

// "Запомнить меня": токен сессии живёт в localStorage, чтобы обновление
// страницы не требовало повторного ввода пароля. Хранить сам пароль нельзя,
// а токен — такой же bearer-секрет, как cookie сессии, поэтому logout явно
// его стирает (см. stopGame).
const SESSION_KEY = "sharolet.session";
function saveSession(token, name) {
  try {
    localStorage.setItem(SESSION_KEY, JSON.stringify({ token, name }));
  } catch {
    // приватный режим / заблокировано — просто не запоминаем, не критично
  }
}
function loadSession() {
  try {
    return JSON.parse(localStorage.getItem(SESSION_KEY));
  } catch {
    return null;
  }
}
function clearSession() {
  try {
    localStorage.removeItem(SESSION_KEY);
  } catch {
    // ignore
  }
}

const hud = document.getElementById("hud");

// ── NPC-диалог: DOM-элементы ──
const npcHint = document.getElementById("npc-hint");
const npcDialog = document.getElementById("npc-dialog");
const npcDialogHeader = npcDialog.querySelector(".npc-title");
const npcDialogClose = npcDialog.querySelector(".close-btn");
const npcDialogMessages = document.getElementById("npc-dialog-messages");
const npcDialogInput = document.getElementById("npc-dialog-input");
const npcDialogSend = document.getElementById("npc-dialog-send");

// ── NPC-диалог: состояние ──
let chatOpen = false; // открыт ли диалог
let chatNpcId = null; // id NPC, с которым говорим
let chatNpcName = null; // имя NPC
let chatWaiting = false; // ждём ответ от сервера
let thinkingEl = null; // элемент "думает..."
let chatRequestId = 0;
let lastConversationHeartbeat = 0;

// The same character in two rooms shares dialogue history, scoped by account.
function getLogicalNpcKey(npcId) {
  if (npcId === "sovunya" || npcId === "sovunya_dining") return "sovunya";
  if (npcId === "losyash" || npcId === "losyash_chemistry") return "losyash";
  return npcId;
}

let dialogueHistories = {};

function getUserScope() {
  return myId || myName || "guest";
}

function getHistory(logicalKey) {
  const user = getUserScope();
  const cacheKey = `${user}:${logicalKey}`;
  if (!dialogueHistories[cacheKey]) {
    try {
      const stored = JSON.parse(sessionStorage.getItem(`sharolyot_chat_${user}_${logicalKey}`) || "[]");
      dialogueHistories[cacheKey] = Array.isArray(stored) ? stored.filter((entry) =>
        typeof entry?.author === "string" && typeof entry.text === "string" && typeof entry.isUser === "boolean"
      ).slice(-60) : [];
    } catch {
      dialogueHistories[cacheKey] = [];
    }
  }
  return dialogueHistories[cacheKey];
}

function appendToHistory(logicalKey, author, text, isUser) {
  const user = getUserScope();
  const list = getHistory(logicalKey);
  list.push({ author, text, isUser });
  if (list.length > 60) list.splice(0, list.length - 60);
  try {
    sessionStorage.setItem(`sharolyot_chat_${user}_${logicalKey}`, JSON.stringify(list));
  } catch { /* The in-memory history still works when storage is unavailable. */ }
}

function clearUserDialogueHistories() {
  dialogueHistories = {};
}

function renderMessageElement(author, text, isUser) {
  const div = document.createElement("div");
  div.className = isUser ? "msg user" : "msg npc";
  div.innerHTML = `<span class="author">${escapeHtml(author)}:</span> ${escapeHtml(text)}`;
  npcDialogMessages.appendChild(div);
  npcDialogMessages.scrollTop = npcDialogMessages.scrollHeight;
}

function requestNpcReply(message) {
  net.send("npc_talk", { npcId: chatNpcId, message, requestId: ++chatRequestId });
}

function updateHud() {
  hud.textContent = disconnected
    ? "Соединение потеряно. Проверь сервер и обнови страницу."
    : !currentRoomBuilt ? "Переход в следующий отсек…"
    : currentRoomBuilt.error || (!currentRoomBuilt.ready
      ? `Загрузка: ${currentRoomBuilt.room.name}…`
      : `${myName} · ${currentRoomBuilt.room.name} · Игроков в комнате: ${players.size + 1}`);
}

// ── Открытие / закрытие диалога ──

function openChat(npcId, npcName) {
  clearMovementKeys();
  chatOpen = true;
  chatNpcId = npcId;
  chatNpcName = npcName;
  chatWaiting = false;
  npcDialogInput.placeholder = npcId.startsWith("sovunya") ? "Например: Дай мне яблоко" : npcId === "krosh" ? "Например: Проводи меня в медблок" : npcId === "nyusha" ? "Например: Сделай мой скафандр синим" : "Спроси что-нибудь…";

  net.send("move", { x: camera.position.x, y: camera.position.y, z: camera.position.z });
  net.setNpcConversation(npcId, true);
  lastConversationHeartbeat = performance.now();

  npcDialogHeader.textContent = npcName;
  npcDialogMessages.innerHTML = "";
  npcDialog.style.display = "flex";
  npcHint.style.display = "none";

  // Разблокируем курсор, чтобы можно было печатать
  if (document.pointerLockElement) {
    document.exitPointerLock();
  }

  // Фокус на поле ввода
  setTimeout(() => npcDialogInput.focus(), 60);

  const history = getHistory(getLogicalNpcKey(npcId));
  if (history.length) {
    for (const message of history) renderMessageElement(message.author, message.text, message.isUser);
  } else {
    addMessage("Вы", "Привет!", true);
    chatWaiting = true;
    showThinking();
    requestNpcReply("Привет!");
  }
}

// restoreLock=false во время logout (stopGame) — там мы уходим на экран
// входа, а не возвращаемся в игру, так что захватывать курсор не нужно.
function closeChat(restoreLock = true) {
  clearMovementKeys();
  if (chatNpcId) net.setNpcConversation(chatNpcId, false);
  chatRequestId++; // Ignore an AI reply from the chat that was just closed.
  chatOpen = false;
  chatNpcId = null;
  chatNpcName = null;
  chatWaiting = false;
  thinkingEl = null;

  npcDialog.style.display = "none";
  npcDialogInput.value = "";
  npcDialogInput.blur();

  if (restoreLock) renderer.domElement.requestPointerLock();
}

// ── Сообщения в чате ──

function addMessage(author, text, isUser) {
  // Убираем "думает..." если есть
  removeThinking();

  renderMessageElement(author, text, isUser);
  if (chatNpcId) appendToHistory(getLogicalNpcKey(chatNpcId), author, text, isUser);
}

function showThinking() {
  removeThinking();
  thinkingEl = document.createElement("div");
  thinkingEl.className = "msg thinking";
  thinkingEl.textContent = `${chatNpcName} думает…`;
  npcDialogMessages.appendChild(thinkingEl);
  npcDialogMessages.scrollTop = npcDialogMessages.scrollHeight;
}

function removeThinking() {
  if (thinkingEl && thinkingEl.parentNode) {
    thinkingEl.parentNode.removeChild(thinkingEl);
  }
  thinkingEl = null;
}

function escapeHtml(str) {
  const div = document.createElement("div");
  div.textContent = str;
  return div.innerHTML;
}

// ── Отправка сообщения игрока ──

function sendPlayerMessage() {
  const text = npcDialogInput.value.trim();
  if (!text || !chatOpen || chatWaiting) return;

  addMessage("Вы", text, true);
  npcDialogInput.value = "";

  chatWaiting = true;
  showThinking();
  requestNpcReply(text);
}

npcDialogSend.addEventListener("click", sendPlayerMessage);

npcDialogInput.addEventListener("keydown", (e) => {
  if (e.key === "Enter") {
    e.preventDefault();
    sendPlayerMessage();
  } else if (e.code === "Escape") {
    // Поле ввода автофокусится при открытии чата, так что это самый частый
    // путь — stopPropagation ниже не даёт событию дойти до window-обработчика,
    // поэтому закрываем прямо здесь, а не полагаемся на всплытие.
    e.preventDefault();
    closeChat();
  }
  // Не даём клавишам WASD/E улетать в game-обработчики
  e.stopPropagation();
});

npcDialogClose.addEventListener("click", closeChat);

// Отвечает NPC в открытом диалоге. Если пришёл ответ, а диалог уже закрыт
// (игрок ушёл/вышел раньше) — просто игнорируем, без отдельного UI.
function showChat(name, text) {
  if (chatOpen && chatNpcId) {
    removeThinking();
    addMessage(name, text, false);
    chatWaiting = false;
    npcDialogInput.disabled = false;
    npcDialogSend.disabled = false;
    setTimeout(() => npcDialogInput.focus(), 30);
  }
}

// Канвас-текстура с именем, billboard (THREE.Sprite всегда смотрит на
// камеру сам, не нужно вручную разворачивать на каждом кадре).
function makeNameTag(name) {
  const canvas = document.createElement("canvas");
  canvas.width = 256;
  canvas.height = 64;
  const ctx = canvas.getContext("2d");
  ctx.fillStyle = "rgba(10, 12, 24, 0.75)";
  ctx.fillRect(0, 16, 256, 32);
  ctx.font = "bold 28px system-ui, sans-serif";
  ctx.textAlign = "center";
  ctx.textBaseline = "middle";
  ctx.fillStyle = "#e8ecff";
  ctx.fillText(name, 128, 33);

  const texture = new THREE.CanvasTexture(canvas);
  const sprite = new THREE.Sprite(new THREE.SpriteMaterial({ map: texture, depthTest: false }));
  sprite.scale.set(1.6, 0.4, 1);
  sprite.position.y = 0.6; // above the astronaut helmet
  sprite.renderOrder = 1; // поверх стен/моделей комнаты
  return sprite;
}

function spawnPlayerMesh(id, name, x, y, z, appearance = {}) {
  const existing = players.get(id);
  if (existing) {
    existing.position.set(x, y, z);
    existing.userData.avatar.setTarget(x, y, z);
    existing.userData.avatar.setWorldState(appearance);
    return existing;
  }
  const mesh = createRemotePlayer(x, y, z);
  mesh.userData.avatar.setWorldState(appearance);
  mesh.add(makeNameTag(name || "?"));
  scene.add(mesh);
  players.set(id, mesh);
  return mesh;
}

function removePlayerMesh(id) {
  const mesh = players.get(id);
  if (!mesh) return;
  scene.remove(mesh);
  for (const child of mesh.children) {
    if (child.material?.map) child.material.map.dispose();
    child.material?.dispose();
  }
  mesh.userData.avatar.dispose();
  players.delete(id);
}

// On transitions, place the player just inside the matching return doorway
// (computeEntryPoint). roster — те, кто уже стоит в комнате, см. ниже.
async function enterChunk(chunk, teleport, roster, sharedState) {
  facilityRoom = chunk.room.id;
  facilityState = sharedState;
  const version = ++roomVersion;
  const previous = currentRoomBuilt;
  currentRoomBuilt = null;
  // Serialize cleanup, including any GLB parse still in flight. The next
  // room cannot start downloading until the previous one's resources are gone.
  roomCleanup = roomCleanup.then(() => destroyRoom(scene, previous));
  for (const id of [...players.keys()]) removePlayerMesh(id);
  if (teleport) {
    camera.position.set(teleport.x, teleport.y ?? 1.6, teleport.z);
    if (teleport.yaw !== undefined) {
      yaw = teleport.yaw;
      pitch = 0;
    }
  }
  net.send("move", { x: camera.position.x, y: camera.position.y, z: camera.position.z });
  if (chatOpen) closeChat();

  // Кто уже стоял в комнате до нас — player_joined присылается только за
  // события ПОСЛЕ нашего входа, без ростера мы бы их никогда не увидели.
  for (const p of roster || []) {
    if (p.playerId === myId) continue;
    spawnPlayerMesh(p.playerId, p.name, p.x, p.y, p.z, p);
  }
  updateHud();
  await roomCleanup;
  if (version !== roomVersion) return;
  renderer.renderLists.dispose();
  currentRoomBuilt = buildRoom(scene, chunk);
  if (facilityState) currentRoomBuilt.applyState(facilityState, true);
  updateHud();
}

// A second client at the identical spawn would be inside the first avatar.
// Pick a nearby free position and face the other player for a clear local demo.
function separateSpawn(position, room, roster = []) {
  const occupied = (x, z) => (roster || []).some((p) => Math.hypot(p.x - x, p.z - z) < 2);
  if (!occupied(position.x, position.z)) return position;
  for (const [dx, dz] of [[0, -3], [3, 0], [0, 3], [-3, 0]]) {
    const x = position.x + dx;
    const z = position.z + dz;
    if (isInsideRoom(room, x, z) && !occupied(x, z)) {
      return { ...position, x, z, yaw: Math.atan2(dx, dz) };
    }
  }
  return position;
}

net.on("welcome", (msg) => {
  myId = msg.playerId;
  myName = msg.name;
  myWorldState = { apples:0, suitColor:"#ffffff", revision:-1 };
  applyPlayerWorldState(msg.playerState || { playerId:myId, apples:0, suitColor:"#ffffff", revision:0 });
  clearUserDialogueHistories();
  disconnected = false;
  applySettings(msg.settings);
  enterChunk(msg.chunk, separateSpawn(msg.position, msg.chunk.room, msg.players), msg.players, msg.state);
});

net.on("chunk_data", (msg) => {
  pendingRoomId = null;
  // position приходит, когда комнату сменил сервер (телепорт из админки),
  // а не игрок прошёл через дверь — тогда точку входа считать не от чего.
  const entry = msg.position ?? computeEntryPoint(msg.chunk.room, fromRoomForTransition);
  fromRoomForTransition = null;
  enterChunk(msg.chunk, entry, msg.players, msg.state);
});

net.on("settings", (msg) => applySettings(msg.settings));

net.on("ping", (msg) => {
  const info = renderer.info.render;
  net.send("pong", {
    id: msg.id,
    fps: Math.round(frameStats.fps * 10) / 10,
    frameMs: Math.round(frameStats.frameMs * 100) / 100,
    memoryMB: performance.memory ? Math.round(performance.memory.usedJSHeapSize / 1048576) : 0,
    drawCalls: info.calls,
    triangles: info.triangles,
    gpu: gpuName,
    paused,
    chatOpen,
  });
});

const announcementEl = document.getElementById("announcement");
let announcementTimer = null;
net.on("announcement", (msg) => {
  announcementEl.textContent = msg.text;
  announcementEl.hidden = false;
  clearTimeout(announcementTimer);
  announcementTimer = setTimeout(() => (announcementEl.hidden = true), 8000);
});

net.on("player_joined", (msg) => {
  if (msg.playerId === myId) return;
  spawnPlayerMesh(msg.playerId, msg.name, msg.x, msg.y, msg.z, msg);
  updateHud();
});

net.on("player_moved", (msg) => {
  const mesh = players.get(msg.playerId);
  if (mesh) mesh.userData.avatar.setTarget(msg.x, msg.y, msg.z);
});

net.on("player_left", (msg) => {
  removePlayerMesh(msg.playerId);
  updateHud();
});

net.on("disconnect", () => {
  disconnected = true;
  for (const id of [...players.keys()]) removePlayerMesh(id);
  updateHud();
});

net.on("npc_response", (msg) => {
  if (msg.npcId === chatNpcId && msg.requestId === chatRequestId) showChat(msg.npcName, msg.text);
});

net.on("error", (msg) => {
  if (msg.code === "item_empty" || msg.code === "item_error") { itemNotice(msg.message); return; }
  if (msg.code === "kicked") {
    stopGame();
    loginError.textContent = msg.message;
    return;
  }
  if (msg.code === "npc_out_of_reach") {
    if (msg.requestId !== undefined && msg.requestId !== chatRequestId) return;
    if (chatOpen) closeChat(false);
    console.warn(msg.message);
    return;
  }
  if (msg.code === "npc_busy") {
    if (chatOpen && msg.requestId === chatRequestId) showChat("Станция", msg.message);
    return;
  }
  pendingRoomId = null;
  fromRoomForTransition = null;
  console.warn("server error:", msg.code, msg.message);
  // Если ждали ответ NPC — снимаем "думает"
  if (chatWaiting) {
    removeThinking();
    chatWaiting = false;
  }
});

// --- движение: WASD + мышь (pointer lock) ---
const keys = {};
window.addEventListener("keydown", (event) => {
  if (event.code !== "KeyF" || event.repeat || !running || paused || chatOpen || disconnected || !currentRoomBuilt?.ready) return;
  net.useApple();
});
window.addEventListener("keydown", (e) => {
  // Не перехватываем клавиши, если чат открыт и фокус на поле ввода
  if (chatOpen && document.activeElement === npcDialogInput) return;
  keys[e.code] = true;
});
window.addEventListener("keyup", (e) => {
  keys[e.code] = false;
});
function clearMovementKeys() {
  for (const key of Object.keys(keys)) keys[key] = false;
}
window.addEventListener("blur", clearMovementKeys);
document.addEventListener("visibilitychange", () => { if (document.hidden) clearMovementKeys(); });

renderer.domElement.addEventListener("click", () => {
  if (!chatOpen && running && !paused) renderer.domElement.requestPointerLock();
});

let yaw = 0;
let pitch = 0;
// Chrome на Windows с захваченным курсором изредка присылает одно событие
// со сдвигом в сотни пикселей — камера "телепортируется". Реальная мышь
// за одно событие (~1–8 мс) так далеко не уезжает, такие выбросы пропускаем.
const MAX_MOUSE_DELTA = 300;
document.addEventListener("mousemove", (e) => {
  if (document.pointerLockElement !== renderer.domElement) return;
  if (Math.abs(e.movementX) > MAX_MOUSE_DELTA || Math.abs(e.movementY) > MAX_MOUSE_DELTA) return;
  yaw -= e.movementX * 0.0025;
  pitch -= e.movementY * 0.0025;
  pitch = Math.max(-1.2, Math.min(1.2, pitch));
});

function findNearestNpc() {
  if (!currentRoomBuilt) return null;
  let closest = null;
  let closestDist = Infinity;
  for (const [id, mesh] of Object.entries(currentRoomBuilt.npcMeshes)) {
    const d = camera.position.distanceTo(mesh.position);
    if (d < closestDist) {
      closestDist = d;
      closest = { id, npc: mesh.userData.npc, dist: d };
    }
  }
  return closest && closest.dist < 3 ? closest : null;
}

// Nearby doorway controls take priority over NPC interaction.
window.addEventListener("keydown", (e) => {
  // Не реагируем на E, если чат уже открыт (чтобы можно было набирать букву "е")
  if (chatOpen) return;
  if (e.code !== "KeyE" || e.repeat || !running || paused || disconnected || pendingRoomId || !currentRoomBuilt?.ready) return;

  const door = currentRoomBuilt.doors.nearest(camera.position, yaw);
  if (door) {
    const change = currentRoomBuilt.doors.toggle(door, camera.position);
    if (change) {
      net.send("move", { x: camera.position.x, y: camera.position.y, z: camera.position.z });
      net.setDoor(currentRoomBuilt.room.id, change.door, change.open);
    }
    return;
  }

  const near = findNearestNpc();
  if (near) {
    const displayName = near.npc?.name || near.id;
    openChat(near.id, displayName);
  }
});

// Стены комнаты блокируют камеру, КРОМЕ узкого "проёма" в направлении
// каждого соседнего отсека (room.connectsTo с сервера). Процедурные комнаты
// (все, кроме хаба) теперь рисуются круглыми с реальной вырезанной аркой
// в стене — см. buildProceduralRoom в world.js, использует те же формулы
// радиуса/ширины прохода, значения должны совпадать с этим файлом.
const WALL_MARGIN = 0.6; // не даём камере влезть прямо в меш стены
const DOOR_HALF_WIDTH = 3; // ширина прохода к соседней комнате

function roomRadius(room) {
  return room.radius || Math.min(room.size.w, room.size.d) / 2;
}

// Authored station rooms supply a circular radius; legacy models use a box.
function isBoxRoom(room) {
  return Boolean(room.model) && !room.radius;
}

function isInsideRoom(room, x, z) {
  const relX = x - room.position.x;
  const relZ = z - room.position.z;
  if (isBoxRoom(room)) {
    const halfW = room.size.w / 2 - WALL_MARGIN;
    const halfD = room.size.d / 2 - WALL_MARGIN;
    return Math.abs(relX) <= halfW && Math.abs(relZ) <= halfD;
  }
  const r = roomRadius(room) - WALL_MARGIN;
  return relX * relX + relZ * relZ <= r * r;
}

function isInDoorway(room, neighbors, x, z) {
  if (room.doors) {
    return room.doors.some((door) => neighbors.some((nb) => nb.id === door.room) && isAtDoor(room, door, x, z));
  }
  for (const nb of neighbors) {
    const dx = nb.position.x - room.position.x;
    const dz = nb.position.z - room.position.z;
    const len = Math.hypot(dx, dz) || 1;
    const dirX = dx / len;
    const dirZ = dz / len;
    const perpX = -dirZ;
    const perpZ = dirX;

    const relX = x - room.position.x;
    const relZ = z - room.position.z;
    const along = relX * dirX + relZ * dirZ; // насколько мы в стороне соседа
    const lateral = relX * perpX + relZ * perpZ; // насколько в стороне от прямой к нему

    if (along > 0 && Math.abs(lateral) < DOOR_HALF_WIDTH) return true;
  }
  return false;
}

function isAtDoor(room, door, x, z, y = camera.position.y) {
  if (Math.abs(y - (room.position.y + door.position.y + 1.6)) > 0.6) return false;
  const { x: dx, z: dz } = door.position;
  const length = Math.hypot(dx, dz) || 1;
  const relX = x - room.position.x;
  const relZ = z - room.position.z;
  const along = (relX * dx + relZ * dz) / length;
  const lateral = (-relX * dz + relZ * dx) / length;
  return along >= length - 1.5 && along <= length + CORRIDOR_DEPTH && Math.abs(lateral) < door.halfWidth + 0.05;
}

// Возвращает (x, z), в которые реально можно встать: как есть, если игрок
// внутри комнаты или в проёме к соседу, иначе — прижатые к ближайшей стене.
function resolveCollision(room, neighbors, x, z) {
  if (isInsideRoom(room, x, z)) return { x, z };
  if (isInDoorway(room, neighbors, x, z)) return { x, z };

  const relX = x - room.position.x;
  const relZ = z - room.position.z;

  if (isBoxRoom(room)) {
    const halfW = room.size.w / 2 - WALL_MARGIN;
    const halfD = room.size.d / 2 - WALL_MARGIN;
    return {
      x: room.position.x + THREE.MathUtils.clamp(relX, -halfW, halfW),
      z: room.position.z + THREE.MathUtils.clamp(relZ, -halfD, halfD),
    };
  }

  const dist = Math.hypot(relX, relZ) || 1;
  const r = roomRadius(room) - WALL_MARGIN;
  return { x: room.position.x + (relX / dist) * r, z: room.position.z + (relZ / dist) * r };
}

// Где встанет игрок, когда появляется в newRoom, придя из fromRoom — чуть
// внутри стены со стороны, откуда он шёл, лицом вглубь новой комнаты.
// Без этого игрок возникал бы в центре комнаты, что при повторном входе
// телепортирует его мимо места, где он реально стоял.
function computeEntryPoint(newRoom, fromRoom) {
  if (!fromRoom) return null;
  const door = newRoom.doors?.find((entry) => entry.room === fromRoom.id);
  if (door) {
    const { x, z } = door.position;
    const length = Math.hypot(x, z) || 1;
    const inset = 1.4;
    return {
      x: newRoom.position.x + x - (x / length) * inset,
      y: newRoom.position.y + door.position.y + 1.6,
      z: newRoom.position.z + z - (z / length) * inset,
      yaw: Math.atan2(x, z),
    };
  }
  const dx = fromRoom.position.x - newRoom.position.x;
  const dz = fromRoom.position.z - newRoom.position.z;
  const len = Math.hypot(dx, dz) || 1;
  const dirX = dx / len;
  const dirZ = dz / len;
  const inset = WALL_MARGIN + 0.8;

  let t;
  if (isBoxRoom(newRoom)) {
    const halfW = newRoom.size.w / 2 - inset;
    const halfD = newRoom.size.d / 2 - inset;
    const tX = dirX !== 0 ? Math.abs(halfW / dirX) : Infinity;
    const tZ = dirZ !== 0 ? Math.abs(halfD / dirZ) : Infinity;
    t = Math.min(tX, tZ);
  } else {
    t = roomRadius(newRoom) - inset;
  }

  return { x: newRoom.position.x + dirX * t, z: newRoom.position.z + dirZ * t };
}

let pendingRoomId = null;
let fromRoomForTransition = null;

// Переход срабатывает в момент пересечения проёма (а не когда игрок дойдёт
// до центра соседней комнаты) — раньше между "прошёл через проём" и
// "комната реально сменилась" была добрая пара секунд ходьбы в пустоте.
function maybeAutoEnterNeighbor() {
  if (!currentRoomBuilt?.ready || pendingRoomId || disconnected) return;
  const { room, neighbors } = currentRoomBuilt;
  if (room.doors) {
    const door = currentRoomBuilt.doors.transitionAt(camera.position);
    if (!door || !neighbors.some((nb) => nb.id === door.room)) return;
    pendingRoomId = door.room;
    fromRoomForTransition = room;
    net.send("move", { x: camera.position.x, y: camera.position.y, z: camera.position.z });
    net.send("chunk_request", { room: door.room });
    return;
  }
  if (isInsideRoom(room, camera.position.x, camera.position.z)) return;

  const nb = neighbors.find((n) => isInDoorway(room, [n], camera.position.x, camera.position.z));
  if (!nb) return;

  pendingRoomId = nb.id;
  fromRoomForTransition = room;
  net.send("chunk_request", { room: nb.id });
}

// Скорости приходят с сервера (settings в welcome и отдельным сообщением,
// когда администратор их меняет); здесь — значения до первого ответа.
let speed = 4; // единиц/сек
let sprintSpeed = 7;
const clock = new THREE.Clock();
let lastSentAt = 0;

// FPS за последнюю секунду — сервер забирает их в ответе на свой ping.
const frameStats = { frames: 0, windowStart: performance.now(), fps: 0, frameMs: 0 };
function countFrame() {
  const now = performance.now();
  frameStats.frames++;
  const elapsed = now - frameStats.windowStart;
  if (elapsed >= 1000) {
    frameStats.fps = (frameStats.frames * 1000) / elapsed;
    frameStats.frameMs = elapsed / frameStats.frames;
    frameStats.frames = 0;
    frameStats.windowStart = now;
  }
}

function applySettings(settings) {
  if (!settings) return;
  if (settings.walkSpeed > 0) speed = settings.walkSpeed;
  if (settings.sprintSpeed > 0) sprintSpeed = settings.sprintSpeed;
}

function tick() {
  if (!running) return; // остановлено логаутом — не планируем следующий кадр

  countFrame();
  const dt = Math.min(clock.getDelta(), 0.05);
  if (!paused) currentRoomBuilt?.doors.update(dt);
  if (!disconnected) currentRoomBuilt?.roombas?.update(dt);
  if (!disconnected) currentRoomBuilt?.npcMotion.update(dt);
  if (!disconnected) currentRoomBuilt?.actions.update(dt, myId);
  const guide = currentRoomBuilt?.sharedState?.npcs?.find((npc) => npc.guidePlayer === myId);
  const guideName = guide && currentRoomBuilt.neighbors.find((room) => room.id === guide.guideRoom)?.name;
  const guideText = guide ? (guide.guideArrived ? `Вот дверь: ${guideName}. Подойди и нажми E.` : chatOpen ? "Крош готов проводить. Закрой чат (Esc)." : `Иди за Крошем → ${guideName}`) : "";
  const guideHint = document.getElementById("guide-hint");
  if (guideHint.textContent !== guideText) guideHint.textContent = guideText;
  if (noticeUntil && performance.now() > noticeUntil) { document.getElementById("item-notice").textContent = ""; noticeUntil = 0; }
  if (!disconnected && chatOpen && performance.now() - lastConversationHeartbeat > 5000) {
    net.setNpcConversation(chatNpcId, true);
    lastConversationHeartbeat = performance.now();
  }

  if (!paused && !chatOpen) {
    camera.rotation.y = yaw;
    camera.rotation.x = pitch;

    const forward = new THREE.Vector3(-Math.sin(yaw), 0, -Math.cos(yaw));
    const right = new THREE.Vector3(-forward.z, 0, forward.x);
    const move = new THREE.Vector3();
    if (keys["KeyW"]) move.add(forward);
    if (keys["KeyS"]) move.sub(forward);
    if (keys["KeyD"]) move.add(right);
    if (keys["KeyA"]) move.sub(right);
    if (move.lengthSq() > 0) {
      const movementSpeed = keys.ShiftLeft || keys.ShiftRight ? sprintSpeed : speed;
      move.normalize().multiplyScalar(movementSpeed * dt);
    }

    if (currentRoomBuilt?.ready && !pendingRoomId && !disconnected) {
      const previousPosition = camera.position.clone();
      if (currentRoomBuilt.collision) currentRoomBuilt.collision.move(camera.position, move);
      else camera.position.add(move);
      const resolved = resolveCollision(currentRoomBuilt.room, currentRoomBuilt.neighbors, camera.position.x, camera.position.z);
      camera.position.x = resolved.x;
      camera.position.z = resolved.z;
      currentRoomBuilt.doors.constrain(previousPosition, camera.position);
      maybeAutoEnterNeighbor();
    }
    updateHud();

    // Подсказка [E] Поговорить — показывается, когда рядом NPC и чат закрыт
    const nearDoor = currentRoomBuilt?.ready ? currentRoomBuilt.doors.nearest(camera.position, yaw) : null;
    npcHint.textContent = nearDoor ? currentRoomBuilt.doors.hint(nearDoor, camera.position) : "[E] Поговорить";
    npcHint.style.display = nearDoor || findNearestNpc() ? "block" : "none";

    const now = performance.now();
    if (now - lastSentAt > 50) {
      lastSentAt = now;
      net.send("move", { x: camera.position.x, y: camera.position.y, z: camera.position.z });
    }
  }

  for (const player of players.values()) player.userData.avatar.update(dt);
  cosmicSky.render(renderer, scene, camera);
  requestAnimationFrame(tick);
}

function startGame() {
  document.getElementById("equipment").hidden = false;
  loginEl.style.display = "none";
  pauseEl.hidden = true;
  paused = false;
  if (!running) {
    running = true;
    tick();
  }
}

// Выход: рвёт соединение (без "потеряно"-сообщения), чистит состояние сцены
// и "запомни меня", возвращает на экран входа.
function stopGame() {
  document.getElementById("equipment").hidden = true;
  carriedApple.visible = false;
  noticeUntil = 0;
  document.getElementById("item-notice").textContent = "";
  facilityRoom = null;
  facilityState = null;
  clearMovementKeys();
  running = false;
  paused = false;
  if (chatOpen) closeChat(false);
  net.disconnect();
  if (document.pointerLockElement) document.exitPointerLock();

  ++roomVersion; // отменяет любой незавершённый enterChunk, не даёт ему достроить комнату после logout
  roomCleanup = roomCleanup.then(() => destroyRoom(scene, currentRoomBuilt));
  currentRoomBuilt = null;
  for (const id of [...players.keys()]) removePlayerMesh(id);
  myId = null;
  myName = null;
  clearUserDialogueHistories();
  disconnected = false;

  clearSession();
  pauseEl.hidden = true;
  loginEl.style.display = "flex";
  loginError.textContent = "";
  loginSubmit.disabled = false;
}

const loginEl = document.getElementById("login");
paintLoginStarfield(loginEl);

const loginForm = document.getElementById("login-form");
const loginError = document.getElementById("login-error");
const loginSubmit = document.getElementById("login-submit");
const loginPasswordHint = document.getElementById("login-password-hint");
const tabs = document.querySelectorAll("#login .tab");

let authMode = "login"; // "login" | "register"
const submitLabel = { login: "Войти на станцию", register: "Зарегистрироваться" };

for (const tab of tabs) {
  tab.addEventListener("click", () => {
    authMode = tab.dataset.mode;
    for (const t of tabs) t.classList.toggle("active", t === tab);
    loginSubmit.textContent = submitLabel[authMode];
    loginPasswordHint.hidden = authMode !== "register";
    loginError.textContent = "";
  });
}

// Та же политика, что сервер проверяет в Register (handlers.go) — здесь
// только для мгновенной подсказки без похода на сервер, финальное слово
// всё равно за сервером.
const PASSWORD_POLICY_MESSAGE = "Пароль должен быть не короче 8 символов и содержать цифру, строчную и заглавную букву";
function passwordPolicyError(password) {
  if (password.length < 8) return PASSWORD_POLICY_MESSAGE;
  if (!/[0-9]/.test(password) || !/[a-zа-яё]/i.test(password) || !/[A-ZА-ЯЁ]/.test(password)) {
    return PASSWORD_POLICY_MESSAGE;
  }
  return null;
}

loginForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = document.getElementById("login-name").value.trim();
  const password = document.getElementById("login-password").value;
  loginError.textContent = "";
  if (!name || !password) {
    loginError.textContent = "Введи имя и пароль";
    return;
  }

  if (authMode === "register") {
    const policyError = passwordPolicyError(password);
    if (policyError) {
      loginError.textContent = policyError;
      return;
    }
  }

  loginSubmit.disabled = true;
  try {
    const { token } = authMode === "register" ? await net.register(name, password) : await net.login(name, password);
    await net.connect(token);
    saveSession(token, name);
    startGame();
  } catch (err) {
    loginError.textContent = err.message;
  } finally {
    loginSubmit.disabled = false;
  }
});

// --- Пауза (Esc во время игры) и выход ---
const pauseEl = document.getElementById("pause");
document.getElementById("pause-resume").addEventListener("click", () => {
  paused = false;
  pauseEl.hidden = true;
  renderer.domElement.requestPointerLock();
});
document.getElementById("pause-exit").addEventListener("click", stopGame);

window.addEventListener("keydown", (e) => {
  if (e.code !== "Escape") return;
  // Открытый диалог с NPC закрывается первым и не открывает паузу поверх него.
  if (chatOpen) {
    e.preventDefault();
    closeChat();
    return;
  }
  if (!running) return;
  paused = !paused;
  clearMovementKeys();
  pauseEl.hidden = !paused;
  if (!paused) renderer.domElement.requestPointerLock();
  else if (document.pointerLockElement) document.exitPointerLock();
});

// "Запомни меня": если в localStorage лежит валидный токен — пробуем
// подключиться им напрямую, без формы логина. Невалидный/просроченный
// токен (сервер ответит отказом на апгрейд WS) просто стирается, и
// экран входа работает как обычно.
(async function tryAutoLogin() {
  const session = loadSession();
  if (!session?.token) return;
  loginSubmit.disabled = true;
  loginError.textContent = `Подключаемся как ${session.name}…`;
  try {
    await net.connect(session.token);
    startGame();
  } catch {
    clearSession();
    loginError.textContent = "";
  } finally {
    loginSubmit.disabled = false;
  }
})();
