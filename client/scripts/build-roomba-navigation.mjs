// Offline asset preparation, not a test. Run from client with:
// node scripts/build-roomba-navigation.mjs
import fs from "node:fs";
import path from "node:path";
import { fileURLToPath } from "node:url";
import * as THREE from "three";
import { GLTFLoader } from "three/addons/loaders/GLTFLoader.js";
import { createRoomCollision } from "../src/collision.js";

const root = path.resolve(path.dirname(fileURLToPath(import.meta.url)), "../..");
const rooms = JSON.parse(fs.readFileSync(path.join(root, "world-data/rooms.json")));
const npcs = JSON.parse(fs.readFileSync(path.join(root, "world-data/npcs.json")));
const loader = new GLTFLoader();
async function geometryOnly(url) {
  const source = fs.readFileSync(path.join(root, "client/public", url));
  const jsonLength = source.readUInt32LE(12);
  const json = JSON.parse(source.toString("utf8", 20, 20 + jsonLength));
  delete json.images; delete json.textures; delete json.materials;
  for (const mesh of json.meshes) for (const primitive of mesh.primitives) delete primitive.material;
  const bytes = Buffer.from(JSON.stringify(json));
  const padded = Buffer.alloc(Math.ceil(bytes.length / 4) * 4, 32);
  bytes.copy(padded);
  const binary = source.subarray(20 + jsonLength);
  const result = Buffer.alloc(20 + padded.length + binary.length);
  source.copy(result, 0, 0, 12);
  result.writeUInt32LE(result.length, 8);
  result.writeUInt32LE(padded.length, 12);
  result.writeUInt32LE(0x4e4f534a, 16);
  padded.copy(result, 20); binary.copy(result, 20 + padded.length);
  return (await loader.parseAsync(result.buffer.slice(result.byteOffset, result.byteOffset + result.length), "")).scene;
}
const roomba = await geometryOnly("models/roomba.glb");
const size = new THREE.Box3().setFromObject(roomba).getSize(new THREE.Vector3());
const height = Math.max(0.1, size.y * 0.48 / Math.max(size.x, size.z));
const navigation = {};
for (const room of Object.values(rooms)) {
  const model = await geometryOnly(room.model);
  model.position.set(room.position.x, room.position.y, room.position.z);
  model.scale.setScalar(room.modelScale || 1);
  const holders = Object.fromEntries(Object.values(npcs).filter(n => n.roomId === room.id)
    .map(n => [n.id, { position: new THREE.Vector3(n.position.x, n.position.y, n.position.z) }]));
  const collision = createRoomCollision(model, holders);
  const radius = (room.radius || Math.min(room.size.w, room.size.d) / 2) - 0.8;
  const points = [], cells = new Map(), coordinates = [];
  function sample(x, z) {
    const dx = x - room.position.x, dz = z - room.position.z;
    if (Math.hypot(dx, dz) > radius) return null;
    for (const door of room.doors || []) {
      const length = Math.hypot(door.position.x, door.position.z);
      if ((dx * door.position.x + dz * door.position.z) / length > length - 1.4 &&
          Math.abs((-dx * door.position.z + dz * door.position.x) / length) < door.halfWidth + 0.7) return null;
    }
    return collision.cleanerFloor(x, z, room.position.y + 0.055, 0.27, height);
  }
  const extent = Math.floor(radius / 0.5);
  for (let ix = -extent; ix <= extent; ix++) for (let iz = -extent; iz <= extent; iz++) {
    const x = room.position.x + ix * 0.5, z = room.position.z + iz * 0.5;
    const y = sample(x, z);
    if (y === null) continue;
    cells.set(`${ix},${iz}`, points.length);
    coordinates.push([ix, iz]);
    points.push({ x, y: +y.toFixed(4), z });
  }
  const links = points.map((point, i) => {
    const [ix, iz] = coordinates[i];
    const neighbors = [];
    for (const [dx, dz] of [[1, 0], [-1, 0], [0, 1], [0, -1]]) {
      const j = cells.get(`${ix + dx},${iz + dz}`);
      if (j === undefined) continue;
      let clear = true;
      for (let step = 1; step <= 10; step++) {
        if (sample(point.x + dx * step * 0.05, point.z + dz * step * 0.05) === null) { clear = false; break; }
      }
      if (clear) neighbors.push(j);
    }
    return neighbors;
  });
  if (links.filter(l => l.length).length < 2) throw new Error(`No cleaner routes in ${room.id}`);
  navigation[room.id] = { points, links };
  console.log(`${room.id}: ${points.length} navigation points`);
  const geometries = new Set(), materials = new Set();
  model.traverse(n => { if (n.geometry) geometries.add(n.geometry); if (n.material) materials.add(n.material); });
  for (const geometry of geometries) geometry.dispose();
  for (const material of materials) material.dispose();
}
fs.writeFileSync(path.join(root, "world-data/roomba-nav.json"), JSON.stringify(navigation) + "\n");
