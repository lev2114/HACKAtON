import * as THREE from "three";

// Only render the current room's authoritative poses. Holder positions are
// shared with collision and E-distance checks, so interaction follows the NPC.
export function createNpcMotion(room, holders, animations) {
  const actors = new Map(Object.entries(holders).map(([id, holder]) => [id, {
    holder, target: holder.position.clone(), heading: 0,
    talkingTo: "", initialized: false, speed: 0,
  }]));
  return {
    applyState(state, instant = false) {
      if (!state || state.room !== room.id) return;
      for (const pose of state.npcs || []) {
        const actor = actors.get(pose.id);
        if (!actor) continue;
        actor.target.set(pose.x, pose.y, pose.z);
        actor.heading = pose.heading;
        actor.talkingTo = pose.talkingTo || "";
        if (instant || !actor.initialized || actor.holder.position.distanceTo(actor.target) > 2) {
          actor.holder.position.copy(actor.target);
          actor.holder.rotation.y = pose.heading;
          actor.speed = 0;
        }
        actor.initialized = true;
      }
    },
    update(dt) {
      if (dt <= 0) return;
      for (const [id, actor] of actors) {
        const { holder } = actor;
        const x = holder.position.x, z = holder.position.z;
        // Keep interpolation tight so it cannot round a path corner far into
        // furniture. A talking actor settles exactly on its stopped pose.
        holder.position.lerp(actor.target, 1 - Math.exp(-24 * dt));
        const speed = Math.hypot(holder.position.x - x, holder.position.z - z) / dt;
        actor.speed = THREE.MathUtils.damp(actor.speed, speed, 12, dt);
        const difference = Math.atan2(Math.sin(actor.heading - holder.rotation.y), Math.cos(actor.heading - holder.rotation.y));
        holder.rotation.y += difference * (1 - Math.exp(-18 * dt));
        animations.get(id)?.update(dt, !!actor.talkingTo, actor.talkingTo ? 0 : actor.speed);
      }
    },
    clear() { actors.clear(); },
  };
}
