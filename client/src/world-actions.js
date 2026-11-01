import * as THREE from "three";
import { createApple, disposeItem } from "./items.js";

export function createRoomActions(group, room) {
  const flights = [];
  const marker = new THREE.Mesh(new THREE.RingGeometry(0.42, 0.50, 32),
    new THREE.MeshBasicMaterial({ color: 0x63e7ff, transparent: true, opacity: 0.8, side: THREE.DoubleSide }));
  marker.rotation.x = -Math.PI / 2;
  marker.visible = false; group.add(marker);
  let state, time = 0;
  return {
    applyState(next) { state = next; },
    event(message) {
      if (message.room !== room.id || message.action !== "give_apple") return;
      const apple = createApple();
      const start = new THREE.Vector3(message.from.x, message.from.y, message.from.z);
      const end = new THREE.Vector3(message.to.x, message.to.y, message.to.z);
      apple.position.copy(start); group.add(apple);
      flights.push({ apple, start, end, elapsed: 0 });
    },
    update(dt, playerId) {
      time += dt;
      const guide = state?.npcs?.find((pose) => pose.guidePlayer === playerId);
      const door = guide && room.doors?.find((entry) => entry.room === guide.guideRoom);
      marker.visible = !!door;
      if (door) {
        const distance = Math.hypot(door.position.x, door.position.z);
        marker.position.set(room.position.x + door.position.x * (1 - 1.8 / distance),
          room.position.y + door.position.y + 0.075,
          room.position.z + door.position.z * (1 - 1.8 / distance));
        marker.scale.setScalar(1 + Math.sin(time * 4) * 0.07);
      }
      for (let i = flights.length - 1; i >= 0; i--) {
        const flight = flights[i]; flight.elapsed += dt;
        const t = Math.min(flight.elapsed / 0.75, 1);
        flight.apple.position.lerpVectors(flight.start, flight.end, t);
        flight.apple.position.y += Math.sin(t * Math.PI) * 0.35;
        flight.apple.rotation.y += dt * 3;
        if (t === 1) { group.remove(flight.apple); disposeItem(flight.apple); flights.splice(i, 1); }
      }
    },
    clear() { flights.length = 0; state = null; },
  };
}
