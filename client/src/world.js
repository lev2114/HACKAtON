// Строит/разбирает геометрию одной комнаты из chunk_data.
// В памяти только текущая комната с NPC; при выходе освобождаем её GLB.
// База фактов NPC остаётся на сервере.
import * as THREE from "three";
import { GLTFLoader } from "three/addons/loaders/GLTFLoader.js";
import { createRoomCollision } from "./collision.js";
import { createDoors } from "./doors.js";
import { createRoombas } from "./roombas.js";

const loader = new GLTFLoader();
async function loadModel(url, built) {
  // No browser asset cache either: only the active room is retained.
  const response = await fetch(url, { signal: built.controller.signal, cache: "no-store" });
  if (!response.ok) throw new Error(`${url}: HTTP ${response.status}`);
  const buffer = await response.arrayBuffer();
  built.controller.signal.throwIfAborted();
  const gltf = await loader.parseAsync(buffer, new URL(".", new URL(url, location.href)).href);
  // Parsing cannot be aborted. A transition waits for this cleanup before
  // fetching the next room, so even a late result cannot accumulate models.
  if (built.disposed) {
    disposeOwnedResources(gltf.scene);
    throw new DOMException("Room unloaded", "AbortError");
  }
  return gltf.scene;
}

// Сгенерированные модели приходят в произвольном масштабе и со своим
// локальным центром — вписываем их в комнату независимо от исходных единиц:
// центрируем по (x, z) комнаты и ставим пол модели на уровень room.position.y.
const TARGET_SPAN = 18;

function fitModelToRoom(root, room) {
  if (room.modelScale) {
    root.scale.setScalar(room.modelScale);
    root.position.set(room.position.x, room.position.y, room.position.z);
    return;
  }
  const box = new THREE.Box3().setFromObject(root);
  const size = box.getSize(new THREE.Vector3());
  const center = box.getCenter(new THREE.Vector3());

  const span = Math.max(size.x, size.z) || 1;
  const scale = TARGET_SPAN / span;
  root.scale.setScalar(scale);

  root.position.set(
    room.position.x - center.x * scale,
    room.position.y - box.min.y * scale,
    room.position.z - center.z * scale
  );
}

export function buildRoom(scene, chunk) {
  const { room, npcs } = chunk;
  const group = new THREE.Group();
  group.name = `room:${room.id}`;
  const built = {
    group, npcMeshes: {}, room, neighbors: chunk.neighbors || [],
    controller: new AbortController(), loads: [], disposed: false,
    ready: !room.model, error: null, collision: null, roombas: null,
  };

  // room.position — это абсолютные мировые координаты (как и у NPC ниже),
  // не локальный ноль группы: иначе стены/пол любой комнаты кроме той,
  // что лежит в (0,0,0), рисуются не там, где на самом деле стоит NPC.
  const { x: rx, y: ry, z: rz } = room.position;

  if (room.model) {
    buildModelRoom(built);
  } else {
    buildProceduralRoom(group, room, rx, ry, rz, chunk.neighbors);
  }

  for (const npc of npcs || []) {
    built.npcMeshes[npc.id] = buildNpcEntity(built, npc);
  }

  addDoorwayLight(group, room, chunk.neighbors);
  built.doors = createDoors(group, room, built.neighbors);
  built.applyState = (state, instant = false) => {
    built.sharedState = state;
    built.doors.applyState(state, instant);
    built.roombas?.applyState(state);
  };

  scene.add(group);
  return built;
}

// Обёртка-Group на npc.position держит стабильную .position (main.js меряет
// расстояние до неё для "поговорить по E") независимо от того, успела ли
// загрузиться 3D-модель — внутри неё либо сразу розовая сфера-заглушка
// (нет npc.model), либо сфера на время загрузки модели, которая потом
// подменяется так же, как модель комнаты (см. buildModelRoom).
function buildNpcEntity(built, npc) {
  const roomGroup = built.group;
  const holder = new THREE.Group();
  holder.position.set(npc.position.x, npc.position.y, npc.position.z);
  holder.userData.npc = npc;
  roomGroup.add(holder);


  const placeholder = new THREE.Mesh(
    new THREE.SphereGeometry(0.8, 16, 16),
    new THREE.MeshStandardMaterial({ color: "#ffb3d9" })
  );
  holder.add(placeholder);

  if (npc.model) {
    built.loads.push(loadModel(npc.model, built)
      .then((instance) => {
        fitNpcModel(instance, npc);

        holder.remove(placeholder);
        placeholder.geometry.dispose();
        placeholder.material.dispose();
        holder.add(instance);
      })
      .catch((err) => {
        if (err.name !== "AbortError") console.error(`не удалось загрузить модель NPC ${npc.id}:`, err);
      }));
  }

  return holder;
}

// Ставит модель персонажа на пол (мировой y=0, как и у всех комнат) и
// масштабирует под разумный рост — модели приходят в произвольном
// исходном масштабе, как и модели комнат (см. fitModelToRoom).
const NPC_TARGET_HEIGHT = 1.7;

function fitNpcModel(root, npc) {
  const box = new THREE.Box3().setFromObject(root);
  const size = box.getSize(new THREE.Vector3());
  const center = box.getCenter(new THREE.Vector3());

  const scale = NPC_TARGET_HEIGHT / (size.y || 1);
  root.scale.setScalar(scale);
  root.position.set(-center.x * scale, -1 - box.min.y * scale, -center.z * scale);
}

// Ширина проёма — должна совпадать с DOOR_HALF_WIDTH в main.js (там же
// коллизия/триггер перехода), иначе видимая дыра в стене и реально
// проходимая зона разъедутся.
const DOOR_HALF_WIDTH = 3;

// Освещает сам проём и кусок черноты за ним, чтобы было видно, куда идти.
// Для процедурных (круглых) комнат геометрия уже сама показывает дыру в
// стене — свет тут просто подсветка, не замена видимости прохода.
function addDoorwayLight(group, room, neighbors) {
  if (room.doors) {
    for (const door of room.doors) {
      const light = new THREE.PointLight(0x4fd8ff, 3, 7);
      light.position.set(room.position.x + door.position.x, room.position.y + 2, room.position.z + door.position.z);
      group.add(light);
    }
    return;
  }
  for (const nb of neighbors || []) {
    const dx = nb.position.x - room.position.x;
    const dz = nb.position.z - room.position.z;
    const len = Math.hypot(dx, dz) || 1;
    const dirX = dx / len;
    const dirZ = dz / len;

    let t;
    if (room.model) {
      const halfW = room.size.w / 2;
      const halfD = room.size.d / 2;
      const tX = dirX !== 0 ? Math.abs(halfW / dirX) : Infinity;
      const tZ = dirZ !== 0 ? Math.abs(halfD / dirZ) : Infinity;
      t = Math.min(tX, tZ);
    } else {
      t = Math.min(room.size.w, room.size.d) / 2;
    }

    const floorY = room.position.y - room.size.h / 2;
    const beacon = new THREE.PointLight(0x4fd8ff, 3, 18);
    beacon.position.set(room.position.x + dirX * t, floorY + 1.6, room.position.z + dirZ * t);
    group.add(beacon);
  }
}

// Круглая комната с настоящей вырезанной аркой в стене (не просто
// "проходимая" зона без видимой дыры) — у всех процедурных комнат ровно
// один сосед (хаб), так что арка всегда одна.
function buildProceduralRoom(group, room, rx, ry, rz, neighbors) {
  const radius = Math.min(room.size.w, room.size.d) / 2;
  const nb = neighbors && neighbors[0];

  let thetaStart = 0;
  let thetaLength = Math.PI * 2;
  if (nb) {
    const dx = nb.position.x - rx;
    const dz = nb.position.z - rz;
    // THREE.CylinderGeometry кладёт вершины как x = r·sin(θ), z = r·cos(θ)
    // (не наоборот) — поэтому atan2(dx, dz), а не atan2(dz, dx).
    const bearing = Math.atan2(dx, dz);
    const halfAngle = Math.atan2(DOOR_HALF_WIDTH, radius);
    // Стена — это дуга ОТ конца проёма до его начала (обходя полный круг
    // в обратную сторону), т.е. "всё, кроме проёма".
    thetaStart = bearing + halfAngle;
    thetaLength = Math.PI * 2 - 2 * halfAngle;
  }

  const wallGeo = new THREE.CylinderGeometry(radius, radius, room.size.h, 48, 1, true, thetaStart, thetaLength);
  const wallMat = new THREE.MeshStandardMaterial({ color: room.color || "#445566", side: THREE.BackSide });
  const wall = new THREE.Mesh(wallGeo, wallMat);
  wall.position.set(rx, ry, rz);
  group.add(wall);

  const floorGeo = new THREE.CircleGeometry(radius, 48);
  const floorMat = new THREE.MeshStandardMaterial({ color: "#1b1f33" });
  const floor = new THREE.Mesh(floorGeo, floorMat);
  floor.rotation.x = -Math.PI / 2;
  floor.position.set(rx, ry - room.size.h / 2 + 0.03, rz);
  group.add(floor);

  const ceilingGeo = new THREE.CircleGeometry(radius, 48);
  const ceilingMat = new THREE.MeshStandardMaterial({ color: room.color || "#445566", side: THREE.DoubleSide });
  const ceiling = new THREE.Mesh(ceilingGeo, ceilingMat);
  ceiling.rotation.x = Math.PI / 2;
  ceiling.position.set(rx, ry + room.size.h / 2 - 0.03, rz);
  group.add(ceiling);
}

function buildModelRoom(built) {
  const { group, room } = built;
  // Заглушка на время загрузки тяжёлой модели (десятки МБ), чтобы игрок не
  // стоял в пустоте, пока она качается и парсится.
  const placeholder = new THREE.Mesh(
    new THREE.BoxGeometry(room.size.w, room.size.h, room.size.d),
    new THREE.MeshStandardMaterial({ color: room.color || "#445566", side: THREE.BackSide })
  );
  placeholder.position.set(room.position.x, room.position.y, room.position.z);
  if (room.modelScale) placeholder.position.y += room.size.h / 2;
  group.add(placeholder);

  built.loads.push(loadModel(room.model, built)
    .then((instance) => {
      fitModelToRoom(instance, room);
      openConnectedDoors(instance, room);
      clearDoorwayTrim(instance, room);
      group.add(instance);
      instance.updateWorldMatrix(true, true);
      if (room.collision === "walkmesh") built.collision = createRoomCollision(instance, built.npcMeshes, [built.doors.corridors]);
      group.remove(placeholder);
      placeholder.geometry.dispose();
      placeholder.material.dispose();
      built.ready = true;
      if (built.collision) {
        built.loads.push(loadModel("/models/roomba.glb", built).then((template) => {
          built.roombas = createRoombas(template, room, built.sharedState);
          group.add(built.roombas.group);
        }).catch((error) => {
          if (error.name !== "AbortError") console.error(`Roomba ${room.id}:`, error);
        }));
      }
    })
    .catch((err) => {
      if (err.name === "AbortError") return;
      built.error = "Не удалось загрузить комнату. Обнови страницу, чтобы повторить.";
      console.error(`не удалось загрузить модель комнаты ${room.id}:`, err);
    }));
}

// Keep the authored frames and destination signs, opening only connected leaves.
// Match GLB extras rather than loader-sanitized node names for the door groups.
function openConnectedDoors(root, room) {
  const destinations = new Set((room.doors || []).map((door) => door.modelDoor).filter(Boolean));
  root.traverse((node) => {
    if (!node.userData.roomDoor || !destinations.has(node.userData.destination)) return;
    node.traverse((part) => {
      if (/destination.plaque|room.label/i.test(part.name)) {
        // Authored door +Z points into the lobby, opposite the corridor.
        // Bring the plaque and its lettering forward together.
        part.position.z += 0.2;
      }
      if (/sliding.panel|cream.lower.inset|destination.color.strip|central.rubber.seal|large.number/i.test(part.name)) {
        part.visible = false;
      }
    });
  });
}

// The authored lower wall trim continues across some door openings. Hide
// only trim segments touching a connected opening, before collecting solids.
// Stair and gallery safety railings remain intact; the source GLB is unchanged.
function clearDoorwayTrim(root, room) {
  root.updateWorldMatrix(true, true);
  const point = new THREE.Vector3();
  root.traverse((node) => {
    if (!node.isMesh || !/lower.dado.seam/i.test(node.name)) return;
    const bounds = new THREE.Box3().setFromObject(node);
    for (const door of room.doors || []) {
      const length = Math.hypot(door.position.x, door.position.z) || 1;
      const nx = door.position.x / length, nz = door.position.z / length;
      const x = room.position.x + door.position.x, z = room.position.z + door.position.z;
      const floorY = room.position.y + door.position.y;
      if (bounds.max.y < floorY || bounds.min.y > floorY + 2.6) continue;
      let minAlong = Infinity, maxAlong = -Infinity, minAcross = Infinity, maxAcross = -Infinity;
      for (const px of [bounds.min.x, bounds.max.x]) {
        for (const pz of [bounds.min.z, bounds.max.z]) {
          point.set(px - x, 0, pz - z);
          const along = point.x * nx + point.z * nz;
          const across = point.x * nz - point.z * nx;
          minAlong = Math.min(minAlong, along); maxAlong = Math.max(maxAlong, along);
          minAcross = Math.min(minAcross, across); maxAcross = Math.max(maxAcross, across);
        }
      }
      const width = door.halfWidth + 0.35;
      if (minAlong <= 0.45 && maxAlong >= -0.45 && minAcross <= width && maxAcross >= -width) {
        node.visible = false;
        break;
      }
    }
  });
}

export async function destroyRoom(scene, built) {
  if (!built) return;
  built.disposed = true;
  built.controller.abort();
  built.doors.closeAll();
  scene.remove(built.group);
  built.collision = null;
  built.doors = null;
  built.roombas = null;
  disposeOwnedResources(built.group);
  built.group.clear();
  built.npcMeshes = {};
  await Promise.allSettled(built.loads);
  built.loads.length = 0;
}

function disposeOwnedResources(obj) {
  const geometries = new Set(), materials = new Set(), textures = new Set(), images = new Set();
  obj.traverse((node) => {
    if (node.geometry) geometries.add(node.geometry);
    if (node.skeleton) node.skeleton.dispose();
    for (const material of node.material ? (Array.isArray(node.material) ? node.material : [node.material]) : []) {
      materials.add(material);
      for (const value of Object.values(material)) if (value?.isTexture) textures.add(value);
    }
  });
  for (const texture of textures) {
    if (texture.source?.data) images.add(texture.source.data);
    texture.dispose();
  }
  for (const image of images) image.close?.();
  for (const material of materials) material.dispose();
  for (const geometry of geometries) geometry.dispose();
}
