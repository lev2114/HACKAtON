import * as THREE from "three";
import { Net } from "./net.js";
import { buildRoom, destroyRoom } from "./world.js";

const net = new Net();

const scene = new THREE.Scene();
scene.background = new THREE.Color(0x05060a);

const camera = new THREE.PerspectiveCamera(70, window.innerWidth / window.innerHeight, 0.1, 500);
camera.position.set(0, 1.6, 5);
camera.rotation.order = "YXZ";

const renderer = new THREE.WebGLRenderer({ antialias: true });
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

const players = new Map(); // playerId -> THREE.Mesh
let currentRoomBuilt = null;
let myId = null;
let myName = null;

const hud = document.getElementById("hud");
const chatBox = document.getElementById("chat");

function showChat(name, text) {
  chatBox.style.display = "block";
  chatBox.innerHTML = `<span class="name">${name}:</span>${text}`;
}

function spawnPlayerMesh(id, x, y, z) {
  const geo = new THREE.CapsuleGeometry(0.4, 1.2, 4, 8);
  const mat = new THREE.MeshStandardMaterial({ color: 0x66ccff });
  const mesh = new THREE.Mesh(geo, mat);
  mesh.position.set(x, y, z);
  scene.add(mesh);
  players.set(id, mesh);
  return mesh;
}

function removePlayerMesh(id) {
  const mesh = players.get(id);
  if (!mesh) return;
  scene.remove(mesh);
  mesh.geometry.dispose();
  mesh.material.dispose();
  players.delete(id);
}

// Игрок физически доходит до новой комнаты сам (см. collision ниже) —
// телепортируем камеру только при первом подключении (teleport = позиция
// игрока из БД), а не при каждой смене чанка, иначе перебьём то место,
// докуда он реально дошёл.
function enterChunk(chunk, teleport) {
  destroyRoom(scene, currentRoomBuilt);
  for (const id of [...players.keys()]) removePlayerMesh(id);
  currentRoomBuilt = buildRoom(scene, chunk);
  if (teleport) camera.position.set(teleport.x, 1.6, teleport.z);
  hud.textContent = `${myName} · ${chunk.room.name}`;
  chatBox.style.display = "none";
}

net.on("welcome", (msg) => {
  myId = msg.playerId;
  myName = msg.name;
  enterChunk(msg.chunk, msg.position);
});

net.on("chunk_data", (msg) => {
  pendingRoomId = null;
  enterChunk(msg.chunk);
});

net.on("player_joined", (msg) => {
  if (msg.playerId === myId) return;
  spawnPlayerMesh(msg.playerId, msg.x, msg.y, msg.z);
});

net.on("player_moved", (msg) => {
  const mesh = players.get(msg.playerId);
  if (mesh) mesh.position.set(msg.x, msg.y, msg.z);
});

net.on("player_left", (msg) => removePlayerMesh(msg.playerId));

net.on("npc_response", (msg) => showChat(msg.npcName, msg.text));

net.on("error", (msg) => console.warn("server error:", msg.code, msg.message));

// --- движение: WASD + мышь (pointer lock) ---
const keys = {};
window.addEventListener("keydown", (e) => (keys[e.code] = true));
window.addEventListener("keyup", (e) => (keys[e.code] = false));

renderer.domElement.addEventListener("click", () => renderer.domElement.requestPointerLock());

let yaw = 0;
let pitch = 0;
document.addEventListener("mousemove", (e) => {
  if (document.pointerLockElement !== renderer.domElement) return;
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

window.addEventListener("keydown", (e) => {
  if (e.code !== "KeyE") return;
  const near = findNearestNpc();
  if (near) net.send("npc_talk", { npcId: near.id, message: "Расскажи что-нибудь интересное" });
});

// Стены комнаты блокируют камеру, КРОМЕ узкого "проёма" в направлении
// каждого соседнего отсека (room.connectsTo с сервера) — так получается и
// коллизия, и физический проход между комнатами одним и тем же механизмом,
// без отдельной геометрии дверей/коридоров.
const WALL_MARGIN = 0.6; // не даём камере влезть прямо в меш стены
const DOOR_HALF_WIDTH = 5; // ширина прохода к соседней комнате — широкая, чтобы прощать неидеальную ходьбу мышью

function isInDoorway(room, neighbors, x, z) {
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

// Возвращает (x, z), в которые реально можно встать: как есть, если игрок
// внутри комнаты или в проёме к соседу, иначе — прижатые к ближайшей стене.
function resolveCollision(room, neighbors, x, z) {
  const halfW = room.size.w / 2 - WALL_MARGIN;
  const halfD = room.size.d / 2 - WALL_MARGIN;
  const relX = x - room.position.x;
  const relZ = z - room.position.z;

  if (Math.abs(relX) <= halfW && Math.abs(relZ) <= halfD) return { x, z };
  if (isInDoorway(room, neighbors, x, z)) return { x, z };

  return {
    x: room.position.x + THREE.MathUtils.clamp(relX, -halfW, halfW),
    z: room.position.z + THREE.MathUtils.clamp(relZ, -halfD, halfD),
  };
}

let pendingRoomId = null;

function maybeAutoEnterNeighbor() {
  if (!currentRoomBuilt || pendingRoomId) return;
  for (const nb of currentRoomBuilt.neighbors) {
    const dx = Math.abs(camera.position.x - nb.position.x);
    const dz = Math.abs(camera.position.z - nb.position.z);
    if (dx < nb.size.w / 2 && dz < nb.size.d / 2) {
      pendingRoomId = nb.id;
      net.send("chunk_request", { room: nb.id });
      return;
    }
  }
}

const speed = 4; // единиц/сек
const clock = new THREE.Clock();
let lastSentAt = 0;

function tick() {
  const dt = clock.getDelta();
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
    move.normalize().multiplyScalar(speed * dt);
    camera.position.add(move);
  }

  if (currentRoomBuilt) {
    const resolved = resolveCollision(currentRoomBuilt.room, currentRoomBuilt.neighbors, camera.position.x, camera.position.z);
    camera.position.x = resolved.x;
    camera.position.z = resolved.z;
    maybeAutoEnterNeighbor();
  }

  const now = performance.now();
  if (now - lastSentAt > 50) {
    lastSentAt = now;
    net.send("move", { x: camera.position.x, y: camera.position.y, z: camera.position.z });
  }

  renderer.render(scene, camera);
  requestAnimationFrame(tick);
}

const loginEl = document.getElementById("login");
const loginForm = document.getElementById("login-form");
loginForm.addEventListener("submit", async (e) => {
  e.preventDefault();
  const name = document.getElementById("login-name").value.trim();
  if (!name) return;
  try {
    const { token } = await net.login(name);
    await net.connect(token);
    loginEl.style.display = "none";
    tick();
  } catch (err) {
    alert("Не удалось подключиться к серверу: " + err.message);
  }
});
