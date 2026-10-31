import * as THREE from "three";
import { Net } from "./net.js";
import { buildRoom, destroyRoom } from "./world.js";

// Временная заглушка вместо реальных дверей/коллизий между отсеками:
// пока игрок физически не "ходит" между комнатами, переход запрашивается
// по номеру клавиши. Заменить на триггер-зоны при проходе через проём —
// см. TASKS.md.
const ROOM_KEYS = {
  Digit0: "hub",
  Digit1: "room_stars",
  Digit2: "room_gravity",
  Digit3: "room_rockets",
  Digit4: "room_greenhouse",
  Digit5: "room_medbay",
};

const net = new Net();

const scene = new THREE.Scene();
scene.background = new THREE.Color(0x05060a);

const camera = new THREE.PerspectiveCamera(70, window.innerWidth / window.innerHeight, 0.1, 500);
camera.position.set(0, 1.6, 5);
camera.rotation.order = "YXZ";

const renderer = new THREE.WebGLRenderer({ antialias: true });
renderer.setSize(window.innerWidth, window.innerHeight);
document.body.appendChild(renderer.domElement);

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

function enterChunk(chunk) {
  destroyRoom(scene, currentRoomBuilt);
  for (const id of [...players.keys()]) removePlayerMesh(id);
  currentRoomBuilt = buildRoom(scene, chunk);
  hud.textContent = `${myName} · ${chunk.room.name}`;
  chatBox.style.display = "none";
}

net.on("welcome", (msg) => {
  myId = msg.playerId;
  myName = msg.name;
  camera.position.set(msg.position.x, 1.6, msg.position.z);
  enterChunk(msg.chunk);
});

net.on("chunk_data", (msg) => enterChunk(msg.chunk));

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
  if (e.code === "KeyE") {
    const near = findNearestNpc();
    if (near) net.send("npc_talk", { npcId: near.id, message: "Расскажи что-нибудь интересное" });
    return;
  }
  const room = ROOM_KEYS[e.code];
  if (room) net.send("chunk_request", { room });
});

const speed = 4; // единиц/сек
const clock = new THREE.Clock();
let lastSentAt = 0;

function tick() {
  const dt = clock.getDelta();
  camera.rotation.y = yaw;
  camera.rotation.x = pitch;

  const forward = new THREE.Vector3(-Math.sin(yaw), 0, -Math.cos(yaw));
  const right = new THREE.Vector3(forward.z, 0, -forward.x);
  const move = new THREE.Vector3();
  if (keys["KeyW"]) move.add(forward);
  if (keys["KeyS"]) move.sub(forward);
  if (keys["KeyD"]) move.add(right);
  if (keys["KeyA"]) move.sub(right);
  if (move.lengthSq() > 0) {
    move.normalize().multiplyScalar(speed * dt);
    camera.position.add(move);
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
