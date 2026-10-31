// Строит/разбирает геометрию одной комнаты из chunk_data.
// Это и есть граница клиентского кеша: в памяти всегда максимум одна
// комната + её NPC-заглушки, а не весь мир и не база фактов NPC.
import * as THREE from "three";

export function buildRoom(scene, chunk) {
  const { room, npcs } = chunk;
  const group = new THREE.Group();
  group.name = `room:${room.id}`;

  const shellGeo = new THREE.BoxGeometry(room.size.w, room.size.h, room.size.d);
  const shellMat = new THREE.MeshStandardMaterial({ color: room.color || "#445566", side: THREE.BackSide });
  group.add(new THREE.Mesh(shellGeo, shellMat));

  const floorGeo = new THREE.PlaneGeometry(room.size.w, room.size.d);
  const floorMat = new THREE.MeshStandardMaterial({ color: "#1b1f33" });
  const floor = new THREE.Mesh(floorGeo, floorMat);
  floor.rotation.x = -Math.PI / 2;
  floor.position.y = -room.size.h / 2;
  group.add(floor);

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

  scene.add(group);
  return { group, npcMeshes, room };
}

export function destroyRoom(scene, built) {
  if (!built) return;
  scene.remove(built.group);
  built.group.traverse((obj) => {
    if (obj.geometry) obj.geometry.dispose();
    if (obj.material) obj.material.dispose();
  });
}
