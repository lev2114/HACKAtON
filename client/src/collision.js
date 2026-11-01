import * as THREE from "three";

export const EYE_HEIGHT = 1.6;
const STEP_HEIGHT = 0.24; // Authored lobby stair risers are 0.19 m.
const MAX_DROP = 0.28;
const BODY_RADIUS = 0.25;
const UP = new THREE.Vector3(0, 1, 0);
const DOWN = new THREE.Vector3(0, -1, 0);

// Use the actual stair wedges and gallery opening, rather than their AABBs:
// a box around the ring would incorrectly fill the atrium and stairwell.
export function createRoomCollision(root, npcMeshes = {}, extraRoots = []) {
  const surfaces = [], solids = [];
  const ray = new THREE.Raycaster();
  const normalMatrix = new THREE.Matrix3();
  const normal = new THREE.Vector3();
  const candidateBox = new THREE.Box3();
  const origin = new THREE.Vector3();
  const direction = new THREE.Vector3();
  const candidate = new THREE.Vector3();
  let needsPlacement = true;
  const radialDirections = Array.from({ length: 8 }, (_, i) =>
    new THREE.Vector3(Math.cos(i * Math.PI / 4), 0, Math.sin(i * Math.PI / 4)));

  for (const collisionRoot of [root, ...extraRoots]) {
    collisionRoot.updateWorldMatrix(true, true);
    collisionRoot.traverseVisible((mesh) => {
      if (!mesh.isMesh) return;
      const entry = { mesh, box: new THREE.Box3().setFromObject(mesh) };
      solids.push(entry);
      if (/continuous.*floor|level.*floor|floor.*disc|Stair.*tread|Gallery.*walking.surface|corridor.*floor|level.connection/i.test(mesh.name)) {
        surfaces.push(entry);
      }
    });
  }
  if (!surfaces.length) throw new Error(`Room model ${root.name} has no walkable surfaces`);

  function floorAt(x, z, feetY) {
    origin.set(x, feetY + STEP_HEIGHT + 0.01, z);
    ray.set(origin, DOWN);
    ray.far = STEP_HEIGHT + MAX_DROP + 0.02;
    let floor = null;
    for (const { mesh, box } of surfaces) {
      if (x < box.min.x || x > box.max.x || z < box.min.z || z > box.max.z) continue;
      for (const hit of ray.intersectObject(mesh, false)) {
        normalMatrix.getNormalMatrix(mesh.matrixWorld);
        normal.copy(hit.face.normal).applyMatrix3(normalMatrix).normalize();
        if (normal.y > 0.7 && (floor === null || hit.point.y > floor)) floor = hit.point.y;
      }
    }
    return floor;
  }

  function blocked(from, to, floor) {
    for (const npc of Object.values(npcMeshes)) {
      if (Math.abs(to.y - npc.position.y) < 1.5 &&
          Math.hypot(to.x - npc.position.x, to.z - npc.position.z) < BODY_RADIUS + 0.48) return true;
    }
    // Broad phase keeps rays away from the hundreds of distant model meshes.
    candidateBox.setFromPoints([from, to]);
    candidateBox.min.y = floor + STEP_HEIGHT + 0.04;
    candidateBox.max.y = floor + EYE_HEIGHT + 0.15;
    candidateBox.min.x -= BODY_RADIUS;
    candidateBox.min.z -= BODY_RADIUS;
    candidateBox.max.x += BODY_RADIUS;
    candidateBox.max.z += BODY_RADIUS;
    const nearby = solids.filter(({ box }) => box.intersectsBox(candidateBox)).map(({ mesh }) => mesh);
    const distance = Math.hypot(to.x - from.x, to.z - from.z);
    direction.set(to.x - from.x, 0, to.z - from.z).normalize();
    const heights = new Set([STEP_HEIGHT + 0.04, 0.5, 0.8, 1.1, 1.4, EYE_HEIGHT + 0.15]);
    // Sample thin tabletops/rails at their own height as well as the body
    // levels, so a slab between two fixed rays cannot be walked through.
    for (const { box } of solids) {
      if (box.intersectsBox(candidateBox)) heights.add(THREE.MathUtils.clamp(
        (box.min.y + box.max.y) / 2 - floor, STEP_HEIGHT + 0.04, EYE_HEIGHT + 0.15));
    }
    for (const height of heights) {
      if (distance > 0) {
        origin.set(from.x, floor + height, from.z);
        ray.set(origin, direction);
        ray.far = distance + BODY_RADIUS;
        if (ray.intersectObjects(nearby, false).length) return true;
      }
      origin.set(to.x, floor + height, to.z);
      for (const radial of radialDirections) {
        ray.set(origin, radial);
        ray.far = BODY_RADIUS;
        if (ray.intersectObjects(nearby, false).length) return true;
      }
    }
    // Prevent stepping up through the underside of the gallery.
    origin.set(to.x, Math.min(from.y - EYE_HEIGHT, floor) + 0.3, to.z);
    ray.set(origin, UP);
    ray.far = floor + EYE_HEIGHT + 0.15 - origin.y;
    return ray.intersectObjects(nearby, false).length > 0;
  }

  function tryStep(position, dx, dz) {
    const floor = floorAt(position.x + dx, position.z + dz, position.y - EYE_HEIGHT);
    // A missing/too-low floor is an edge: stay behind the gallery railing.
    if (floor === null) return false;
    candidate.set(position.x + dx, floor + EYE_HEIGHT, position.z + dz);
    if (blocked(position, candidate, floor)) return false;
    position.copy(candidate);
    return true;
  }

  return {
    cleanerFloor(x, z, feetY, radius = 0.25, height = 0.22) {
      const floor = floorAt(x, z, feetY);
      if (floor === null || Math.abs(floor - feetY) > 0.06) return null;
      for (const npc of Object.values(npcMeshes)) {
        if (Math.abs(npc.position.y - floor - 1) < 0.5 &&
            Math.hypot(npc.position.x - x, npc.position.z - z) < radius + 0.55) return null;
      }
      candidateBox.min.set(x - radius, floor + 0.03, z - radius);
      candidateBox.max.set(x + radius, floor + height, z + radius);
      const nearby = solids.filter(({ box }) => box.intersectsBox(candidateBox)).map(({ mesh }) => mesh);
      origin.set(x, floor + height / 2, z);
      if (solids.some(({ box }) => box.intersectsBox(candidateBox) && box.containsPoint(origin))) return null;
      for (const y of [floor + 0.04, floor + height / 2, floor + height]) {
        origin.set(x, y, z);
        for (const direction of radialDirections) {
          ray.set(origin, direction);
          ray.far = radius + 0.04;
          if (ray.intersectObjects(nearby, false).length) return null;
        }
      }
      return floor;
    },
    move(position, delta) {
      const floor = floorAt(position.x, position.z, position.y - EYE_HEIGHT);
      if (floor !== null) position.y = floor + EYE_HEIGHT;
      // A roster offset or a saved position can be inside newly added furniture.
      if (needsPlacement && floor !== null && blocked(position, position, floor)) {
        placement: for (let radius = 0.5; radius <= 3; radius += 0.5) {
          for (const radial of radialDirections) {
            candidate.copy(position).addScaledVector(radial, radius);
            const nearbyFloor = floorAt(candidate.x, candidate.z, floor);
            if (nearbyFloor === null) continue;
            candidate.y = nearbyFloor + EYE_HEIGHT;
            if (!blocked(candidate, candidate, nearbyFloor)) {
              position.copy(candidate);
              break placement;
            }
          }
        }
      }
      needsPlacement = false;
      const steps = Math.ceil(Math.hypot(delta.x, delta.z) / 0.08);
      if (!steps) return;
      const dx = delta.x / steps, dz = delta.z / steps;
      for (let i = 0; i < steps; i++) {
        if (!tryStep(position, dx, dz)) {
          // Sliding along a wall also makes the curved staircase forgiving.
          if (dx) tryStep(position, dx, 0);
          if (dz) tryStep(position, 0, dz);
        }
      }
    },
  };
}
