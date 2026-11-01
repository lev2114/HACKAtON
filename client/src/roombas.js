import * as THREE from "three";

const RADIUS = 0.24;
const MODEL_YAW_OFFSET = -Math.PI / 2;

// Roombas are server actors. Clients only build their models and interpolate
// snapshots; there is no local random movement or obstacle decision.
export function createRoombas(template, room, initialState) {
  const group = new THREE.Group();
  group.name = `roombas:${room.id}`;
  template.updateMatrixWorld(true);
  const box = new THREE.Box3().setFromObject(template);
  const size = box.getSize(new THREE.Vector3());
  const center = box.getCenter(new THREE.Vector3());
  const scale = RADIUS * 2 / (Math.max(size.x, size.z) || 1);
  template.visible = false;
  group.add(template);
  const robots = new Map();
  function applyState(state) {
    if (!state || state.room !== room.id) return;
    for (const pose of state.roombas) {
      let robot = robots.get(pose.id);
      if (!robot) {
        const body = new THREE.Group();
        body.position.set(pose.x, pose.y, pose.z);
        const fitted = new THREE.Group();
        fitted.scale.setScalar(scale);
        fitted.position.set(-center.x * scale, -box.min.y * scale, -center.z * scale);
        const instance = template.clone(true);
        instance.visible = true;
        fitted.add(instance);
        body.add(fitted);
        body.rotation.y = pose.heading + MODEL_YAW_OFFSET;
        group.add(body);
        robot = { body, target: body.position.clone(), heading: pose.heading };
        robots.set(pose.id, robot);
      }
      robot.target.set(pose.x, pose.y, pose.z);
      robot.heading = pose.heading;
      if (robot.body.position.distanceTo(robot.target) > 1) robot.body.position.copy(robot.target);
    }
  }
  applyState(initialState);
  return {
    group, applyState,
    update(dt) {
      for (const robot of robots.values()) {
        robot.body.position.lerp(robot.target, 1 - Math.exp(-20 * dt));
        const targetAngle = robot.heading + MODEL_YAW_OFFSET;
        const difference = Math.atan2(Math.sin(targetAngle - robot.body.rotation.y), Math.cos(targetAngle - robot.body.rotation.y));
        robot.body.rotation.y += difference * (1 - Math.exp(-20 * dt));
      }
    },
  };
}
