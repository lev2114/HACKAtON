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
    buildProceduralRoom(group, room, rx, ry, rz);
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

  buildDoorwayMarkers(group, room, chunk.neighbors);

  scene.add(group);
  return { group, npcMeshes, room, neighbors: chunk.neighbors || [] };
}

// Проход между комнатами логический (нет геометрии коридора), поэтому без
// подсказки игрок просто идёт в черноту и не понимает, туда ли он идёт.
// Ставим светящийся маяк ровно в точке, где невидимый проём пересекает
// стену (см. isInDoorway/resolveCollision в main.js — направление и
// ширина прохода должны совпадать с этой же математикой).
function buildDoorwayMarkers(group, room, neighbors) {
  for (const nb of neighbors || []) {
    const dx = nb.position.x - room.position.x;
    const dz = nb.position.z - room.position.z;
    const len = Math.hypot(dx, dz) || 1;
    const dirX = dx / len;
    const dirZ = dz / len;

    const halfW = room.size.w / 2;
    const halfD = room.size.d / 2;
    const tX = dirX !== 0 ? Math.abs(halfW / dirX) : Infinity;
    const tZ = dirZ !== 0 ? Math.abs(halfD / dirZ) : Infinity;
    const t = Math.min(tX, tZ);

    const markerX = room.position.x + dirX * t;
    const markerZ = room.position.z + dirZ * t;
    const floorY = room.position.y - room.size.h / 2;
    const markerHeight = room.size.h * 0.8;

    const marker = new THREE.Mesh(
      new THREE.CylinderGeometry(0.2, 0.2, markerHeight, 10),
      new THREE.MeshStandardMaterial({ color: "#8be9ff", emissive: "#4fd8ff", emissiveIntensity: 1.5 })
    );
    marker.position.set(markerX, floorY + markerHeight / 2, markerZ);
    group.add(marker);

    // Освещает и сам проём, и кусок черноты за ним — чтобы было видно,
    // куда идти, ещё до того, как дойдёшь до маяка.
    const beacon = new THREE.PointLight(0x4fd8ff, 3, 18);
    beacon.position.set(markerX, floorY + 1.6, markerZ);
    group.add(beacon);
  }
}

function buildProceduralRoom(group, room, rx, ry, rz) {
  const shellGeo = new THREE.BoxGeometry(room.size.w, room.size.h, room.size.d);
  const shellMat = new THREE.MeshStandardMaterial({ color: room.color || "#445566", side: THREE.BackSide });
  const shell = new THREE.Mesh(shellGeo, shellMat);
  shell.position.set(rx, ry, rz);
  group.add(shell);

  const floorGeo = new THREE.PlaneGeometry(room.size.w, room.size.d);
  const floorMat = new THREE.MeshStandardMaterial({ color: "#1b1f33" });
  const floor = new THREE.Mesh(floorGeo, floorMat);
  floor.rotation.x = -Math.PI / 2;
  // Чуть выше внутренней грани короба комнаты, иначе две копланарные
  // поверхности мерцают (z-fighting) при любом малейшем движении камеры.
  floor.position.set(rx, ry - room.size.h / 2 + 0.03, rz);
  group.add(floor);
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
