// Строит/разбирает геометрию одной комнаты из chunk_data.
// Это и есть граница клиентского кеша: в памяти всегда максимум одна
// комната + её NPC-заглушки, а не весь мир и не база фактов NPC.
import * as THREE from "three";
import { GLTFLoader } from "three/addons/loaders/GLTFLoader.js";

const loader = new GLTFLoader();
// url -> Promise<THREE.Object3D>: модель парсится один раз и переиспользуется
// клонированием, а не перекачивается заново при каждом входе в комнату.
const modelCache = new Map();

function loadModel(url) {
  if (!modelCache.has(url)) {
    modelCache.set(
      url,
      new Promise((resolve, reject) => {
        loader.load(url, (gltf) => resolve(gltf.scene), undefined, reject);
      })
    );
  }
  return modelCache.get(url);
}

// Сгенерированные модели приходят в произвольном масштабе и со своим
// локальным центром — вписываем их в комнату независимо от исходных единиц:
// центрируем по (x, z) комнаты и ставим пол модели на уровень room.position.y.
const TARGET_SPAN = 18;

function fitModelToRoom(root, room) {
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

  // room.position — это абсолютные мировые координаты (как и у NPC ниже),
  // не локальный ноль группы: иначе стены/пол любой комнаты кроме той,
  // что лежит в (0,0,0), рисуются не там, где на самом деле стоит NPC.
  const { x: rx, y: ry, z: rz } = room.position;

  if (room.model) {
    buildModelRoom(scene, group, room);
  } else {
    buildProceduralRoom(group, room, rx, ry, rz, chunk.neighbors);
  }

  const npcMeshes = {};
  for (const npc of npcs || []) {
    const npcGeo = new THREE.SphereGeometry(0.8, 16, 16);
    const npcMat = new THREE.MeshStandardMaterial({ color: "#ffb3d9" });
    const mesh = new THREE.Mesh(npcGeo, npcMat);
    mesh.position.set(npc.position.x, npc.position.y, npc.position.z);
    mesh.userData.npc = npc;
    group.add(mesh);
    npcMeshes[npc.id] = mesh;
  }

  addDoorwayLight(group, room, chunk.neighbors);

  scene.add(group);
  return { group, npcMeshes, room, neighbors: chunk.neighbors || [] };
}

// Ширина проёма — должна совпадать с DOOR_HALF_WIDTH в main.js (там же
// коллизия/триггер перехода), иначе видимая дыра в стене и реально
// проходимая зона разъедутся.
const DOOR_HALF_WIDTH = 3;

// Освещает сам проём и кусок черноты за ним, чтобы было видно, куда идти.
// Для процедурных (круглых) комнат геометрия уже сама показывает дыру в
// стене — свет тут просто подсветка, не замена видимости прохода.
function addDoorwayLight(group, room, neighbors) {
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

function buildModelRoom(scene, group, room) {
  // Заглушка на время загрузки тяжёлой модели (десятки МБ), чтобы игрок не
  // стоял в пустоте, пока она качается и парсится.
  const placeholder = new THREE.Mesh(
    new THREE.BoxGeometry(room.size.w, room.size.h, room.size.d),
    new THREE.MeshStandardMaterial({ color: room.color || "#445566", side: THREE.BackSide })
  );
  placeholder.position.set(room.position.x, room.position.y, room.position.z);
  group.add(placeholder);

  loadModel(room.model)
    .then((sceneRoot) => {
      if (group.parent !== scene) return; // игрок уже вышел из комнаты, не вставляем в никуда

      const instance = sceneRoot.clone(true);
      // Геометрия/материалы у клона общие с закэшированным оригиналом —
      // destroyRoom() не должен их dispose(), иначе следующий визит в эту
      // комнату отрисует уже уничтоженные GPU-ресурсы. См. пометку ниже.
      instance.userData.sharedModel = true;
      fitModelToRoom(instance, room);

      group.remove(placeholder);
      placeholder.geometry.dispose();
      placeholder.material.dispose();
      group.add(instance);
    })
    .catch((err) => console.error(`не удалось загрузить модель комнаты ${room.id}:`, err));
}

export function destroyRoom(scene, built) {
  if (!built) return;
  scene.remove(built.group);
  disposeOwnedResources(built.group);
}

function disposeOwnedResources(obj) {
  if (obj.userData.sharedModel) return; // общий кеш — не трогаем его geometry/material
  if (obj.geometry) obj.geometry.dispose();
  if (obj.material) {
    const materials = Array.isArray(obj.material) ? obj.material : [obj.material];
    materials.forEach((m) => m.dispose());
  }
  for (const child of obj.children) disposeOwnedResources(child);
}
