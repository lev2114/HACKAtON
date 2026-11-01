import * as THREE from "three";

export const CORRIDOR_DEPTH = 3;
export const CORRIDOR_TRANSITION = CORRIDOR_DEPTH / 2;
const CLEARANCE = 0.4;

// Server snapshots own door state; this object only animates the current room.
export function createDoors(group, room, neighbors) {
  const corridors = new THREE.Group();
  corridors.name = "door-corridors";
  group.add(corridors);
  const doors = (room.doors || []).map((entry) => {
    const height = room.id === "hub" ? 2.8 : 3.12;
    const halfSpan = entry.halfWidth + 0.26;
    const position = new THREE.Vector3(
      room.position.x + entry.position.x,
      room.position.y + entry.position.y,
      room.position.z + entry.position.z);
    const normal = new THREE.Vector3(entry.position.x, 0, entry.position.z).normalize();
    const tangent = new THREE.Vector3(normal.z, 0, -normal.x);
    const frame = new THREE.Group();
    frame.name = `door:${room.id}:${entry.room}`;
    frame.position.copy(position);
    frame.rotation.y = Math.atan2(normal.x, normal.z);
    group.add(frame);
    const metal = new THREE.MeshStandardMaterial({ color: 0xe2ddd0, metalness: 0.45, roughness: 0.5 });
    const teal = new THREE.MeshStandardMaterial({ color: 0x23565f, metalness: 0.5, roughness: 0.4 });
    const indicator = new THREE.MeshStandardMaterial({ color: 0xff9566, emissive: 0xff5722, emissiveIntensity: 0.6 });
    const corridor = new THREE.Group();
    corridor.position.copy(position);
    corridor.rotation.copy(frame.rotation);
    corridors.add(corridor);
    const wallMaterial = new THREE.MeshStandardMaterial({ color: 0x294b52, roughness: 0.8 });
    const deckMaterial = new THREE.MeshStandardMaterial({
      color: 0x59656b, roughness: 0.9,
      // Prefer this surface where the authored floor is coplanar, without
      // moving the walkable geometry or creating a step at the threshold.
      polygonOffset: true, polygonOffsetFactor: -1, polygonOffsetUnits: -2,
    });
    const lightMaterial = new THREE.MeshStandardMaterial({ color: 0xb6fff3, emissive: 0x75dacb, emissiveIntensity: 1 });
    function box(name, w, h, d, x, y, z, material) {
      const mesh = new THREE.Mesh(new THREE.BoxGeometry(w, h, d), material);
      mesh.name = name;
      mesh.position.set(x, y, z);
      corridor.add(mesh);
    }
    const width = halfSpan * 2;
    const ceilingStart = 0.18;
    const wallStart = 0;
    // An opaque sleeve and end bulkhead conceal space without loading a
    // neighboring room. The transition happens before the end bulkhead.
    box("Door corridor floor", width + 0.2, 0.12, CORRIDOR_DEPTH + 0.4, 0, -0.065, 1.3, deckMaterial);
    // Keep the ceiling/walls behind the authored lintel and name plaque.
    // Extending them into the room covered the signs above the doorway.
    box("Door corridor ceiling", width + 0.2, 0.16, CORRIDOR_DEPTH - ceilingStart,
      0, height + 0.08, (CORRIDOR_DEPTH + ceilingStart) / 2, wallMaterial);
    for (const side of [-1, 1]) {
      box("Door corridor wall", 0.2, height, CORRIDOR_DEPTH - wallStart,
        side * (halfSpan + 0.1), height / 2, (CORRIDOR_DEPTH + wallStart) / 2, wallMaterial);
      box("Corridor light strip", 0.035, 0.07, CORRIDOR_DEPTH - 0.3, side * (halfSpan - 0.025), 1.1, 1.4, lightMaterial);
    }
    box("Corridor end bulkhead", width + 0.4, height + 0.2, 0.18, 0, height / 2, CORRIDOR_DEPTH, wallMaterial);
    box("Corridor end inset", width * 0.8, height * 0.75, 0.025, 0, height / 2, CORRIDOR_DEPTH - 0.11, metal);
    const light = new THREE.PointLight(0xc6fff4, 5, 5);
    light.position.set(0, height - 0.3, 1.5);
    corridor.add(light);
    const panels = [];
    for (const side of [-1, 1]) {
      const panel = new THREE.Group();
      const mesh = new THREE.Mesh(new THREE.BoxGeometry(halfSpan + 0.03, height + 0.06, 0.18), metal);
      mesh.position.y = height / 2 - 0.02;
      panel.add(mesh);
      const inset = new THREE.Mesh(new THREE.BoxGeometry(entry.halfWidth * 0.76, 1.65, 0.2), teal);
      inset.position.y = height / 2;
      panel.add(inset);
      const strip = new THREE.Mesh(new THREE.BoxGeometry(entry.halfWidth * 0.75, 0.065, 0.22), indicator);
      strip.position.y = 1.65;
      panel.add(strip);
      panel.position.x = side * halfSpan / 2;
      frame.add(panel);
      panels.push({ panel, side });
    }
    return { entry, position, normal, tangent, panels, halfSpan, indicator, open: false, progress: 0,
      name: neighbors.find((neighbor) => neighbor.id === entry.room)?.name || entry.room };
  });

  function coordinates(door, position) {
    const dx = position.x - door.position.x, dz = position.z - door.position.z;
    return { along: dx * door.normal.x + dz * door.normal.z,
      across: dx * door.tangent.x + dz * door.tangent.z };
  }
  function sameLevel(door, position) {
    return Math.abs(position.y - door.position.y - 1.6) < 0.75;
  }
  function occupied(door, position) {
    const local = coordinates(door, position);
    return sameLevel(door, position) && Math.abs(local.along) < 0.65 &&
      Math.abs(local.across) < door.entry.halfWidth + 0.3;
  }

  return {
    corridors,
    nearest(position, yaw) {
      let nearest = null, distance = 2.5;
      for (const door of doors) {
        if (!sameLevel(door, position)) continue;
        const local = coordinates(door, position);
        if (Math.abs(local.across) > door.entry.halfWidth + 0.6) continue;
        const dx = door.position.x - position.x, dz = door.position.z - position.z;
        const d = Math.hypot(dx, dz);
        if (d > distance || (d > 0.3 && (-Math.sin(yaw) * dx - Math.cos(yaw) * dz) / d < 0.25)) continue;
        nearest = door;
        distance = d;
      }
      return nearest;
    },
    hint(door, position) {
      if (door.open && occupied(door, position)) return "Отойди от проёма, чтобы закрыть дверь";
      return `[E] ${door.open ? "Закрыть" : "Открыть"} дверь · ${door.name}`;
    },
    toggle(door, position) {
      if (door.open && occupied(door, position)) return null;
      return { door: door.entry.room, open: !door.open };
    },
    applyState(state, instant = false) {
      for (const door of doors) {
        const pose = state.doors.find((entry) => entry.room === door.entry.room);
        if (!pose) continue;
        door.open = pose.open;
        if (instant) {
          door.progress = pose.open ? 1 : 0;
          for (const { panel, side } of door.panels) {
            panel.position.x = side * (door.halfSpan / 2 + (door.halfSpan + 0.2) * door.progress);
          }
        }
      }
    },
    transitionAt(position) {
      return doors.find((door) => {
        if (!door.open || door.progress < 0.99 || !sameLevel(door, position)) return false;
        const local = coordinates(door, position);
        return local.along >= CORRIDOR_TRANSITION && local.along <= CORRIDOR_DEPTH &&
          Math.abs(local.across) <= door.entry.halfWidth + 0.05;
      })?.entry;
    },
    update(dt) {
      for (const door of doors) {
        door.progress = THREE.MathUtils.clamp(door.progress + (door.open ? 1 : -1) * dt * 2.8, 0, 1);
        const t = door.progress * door.progress * (3 - 2 * door.progress);
        for (const { panel, side } of door.panels) {
          panel.position.x = side * (door.halfSpan / 2 + (door.halfSpan + 0.2) * t);
        }
        const color = door.open && door.progress >= 0.99 ? 0x52e6b5 : 0xff9566;
        door.indicator.color.setHex(color);
        door.indicator.emissive.setHex(color);
      }
    },
    constrain(from, to) {
      for (const door of doors) {
        if (door.open && door.progress >= 0.99 || !sameLevel(door, to)) continue;
        const a = coordinates(door, from), b = coordinates(door, to);
        if (Math.abs(b.along) >= CLEARANCE && a.along * b.along > 0) continue;
        const t = Math.abs(b.along - a.along) > 1e-6
          ? THREE.MathUtils.clamp(-a.along / (b.along - a.along), 0, 1) : 1;
        const across = THREE.MathUtils.lerp(a.across, b.across, t);
        if (Math.abs(across) > door.entry.halfWidth + 0.3) continue;
        const limit = (a.along > 0 ? 1 : -1) * CLEARANCE;
        to.x += door.normal.x * (limit - b.along);
        to.z += door.normal.z * (limit - b.along);
      }
    },
    closeAll() {
      for (const door of doors) {
        door.open = false;
        door.progress = 0;
        for (const { panel, side } of door.panels) panel.position.x = side * door.halfSpan / 2;
      }
    },
  };
}
