import * as THREE from "three";
import { GLTFLoader } from "three/addons/loaders/GLTFLoader.js";
import { clone } from "three/addons/utils/SkeletonUtils.js";

const loader = new GLTFLoader();
const AVATAR_HEIGHT = 1.6;
let templatePromise;
function loadAstronaut() {
  if (!templatePromise) {
    templatePromise = loader.loadAsync("/models/astronaut-player.glb")
      .then((gltf) => gltf.scene)
      .catch((error) => { templatePromise = null; throw error; });
  }
  return templatePromise;
}

function prepareRig(model) {
  const bones = {};
  model.traverse((node) => { if (node.isBone) bones[node.name] = node; });
  const find = (name) => Object.values(bones).find((bone) => bone.name.replace(/_/g, " ").startsWith(name));
  // SkinnedMesh overrides updateMatrixWorld to refresh bindMatrixInverse.
  // updateWorldMatrix bypasses that override after SkeletonUtils.clone,
  // giving rotated bounds and an offset pivot when fitting the astronaut.
  model.updateMatrixWorld(true);
  // The source is in a T pose. Lower each shoulder in world space, preserving
  // its authored bone axes, before recording the neutral pose for the gait.
  for (const side of ["Left", "Right"]) {
    const arm = find(`${side} arm`), elbow = find(`${side} elbow`);
    if (!arm || !elbow) continue;
    const from = elbow.getWorldPosition(new THREE.Vector3()).sub(arm.getWorldPosition(new THREE.Vector3())).normalize();
    const down = new THREE.Vector3(side === "Left" ? 0.12 : -0.12, -1, 0).normalize();
    const world = arm.getWorldQuaternion(new THREE.Quaternion());
    world.premultiply(new THREE.Quaternion().setFromUnitVectors(from, down));
    const parentInverse = arm.parent.getWorldQuaternion(new THREE.Quaternion()).invert();
    arm.quaternion.copy(parentInverse.multiply(world));
    model.updateMatrixWorld(true);
  }
  const joints = {};
  for (const side of ["Left", "Right"]) {
    for (const part of ["arm", "elbow", "leg", "knee", "ankle"]) {
      const bone = find(`${side} ${part}`);
      if (!bone) continue;
      const parentInverse = bone.parent.getWorldQuaternion(new THREE.Quaternion()).invert();
      joints[`${side} ${part}`] = {
        bone, rest: bone.quaternion.clone(),
        axis: new THREE.Vector3(1, 0, 0).applyQuaternion(parentInverse),
      };
    }
  }
  return joints;
}

export function createRemotePlayer(x, y, z) {
  const root = new THREE.Group();
  root.position.set(x, y, z); // Network coordinates are camera/eye coordinates.
  const visual = new THREE.Group();
  visual.position.y = -1.6;
  root.add(visual);
  const placeholder = new THREE.Mesh(new THREE.CapsuleGeometry(0.3, 1, 4, 8),
    new THREE.MeshStandardMaterial({ color: 0xc9dbe6 }));
  placeholder.position.y = AVATAR_HEIGHT / 2;
  visual.add(placeholder);
  const target = root.position.clone();
  let disposed = false, model, joints = {}, phase = 0, gaitSpeed = 0;
  let placeholderDisposed = false;
  const rotation = new THREE.Quaternion();
  function removePlaceholder() {
    if (placeholderDisposed) return;
    placeholderDisposed = true;
    visual.remove(placeholder);
    placeholder.geometry.dispose();
    placeholder.material.dispose();
  }
  loadAstronaut().then((template) => {
    if (disposed) return;
    model = clone(template); // Independent skeleton; geometry/textures shared.
    joints = prepareRig(model);
    const box = new THREE.Box3().setFromObject(model);
    const scale = AVATAR_HEIGHT / (box.max.y - box.min.y || 1);
    const center = box.getCenter(new THREE.Vector3());
    // Fit outside the authored hierarchy, keeping bones and mesh together.
    // visual rotates around the centered feet, never around a displaced GLB.
    const fitted = new THREE.Group();
    fitted.scale.setScalar(scale);
    fitted.position.set(-center.x * scale, -box.min.y * scale, -center.z * scale);
    fitted.add(model);
    removePlaceholder();
    visual.add(fitted);
    root.updateMatrixWorld(true);
  }).catch((error) => {
    if (!disposed) console.error("Не удалось загрузить модель игрока:", error);
  });

  root.userData.avatar = {
    setTarget(x, y, z) {
      target.set(x, y, z);
      if (root.position.distanceTo(target) > 4) {
        root.position.copy(target); // Room teleports are not walking strides.
        gaitSpeed = 0;
      }
    },
    update(dt) {
      if (dt <= 0 || disposed) return;
      const previousX = root.position.x, previousZ = root.position.z;
      root.position.lerp(target, 1 - Math.exp(-18 * dt));
      if (root.position.distanceToSquared(target) < 0.000004) root.position.copy(target);
      const dx = root.position.x - previousX, dz = root.position.z - previousZ;
      const speed = Math.min(9, Math.hypot(dx, dz) / dt);
      gaitSpeed = THREE.MathUtils.damp(gaitSpeed, speed > 0.08 ? speed : 0, 14, dt);
      if (speed > 0.1) {
        const angle = Math.atan2(dx, dz);
        const difference = Math.atan2(Math.sin(angle - visual.rotation.y), Math.cos(angle - visual.rotation.y));
        visual.rotation.y += difference * (1 - Math.exp(-14 * dt));
      }
      const moving = THREE.MathUtils.clamp(gaitSpeed / 1.2, 0, 1);
      const sprint = THREE.MathUtils.clamp((gaitSpeed - 4) / 3, 0, 1);
      phase += gaitSpeed * dt * Math.PI * 2 / THREE.MathUtils.lerp(1.8, 2.5, sprint);
      function pose(name, angle) {
        const joint = joints[name];
        if (!joint) return;
        joint.bone.quaternion.copy(joint.rest).premultiply(rotation.setFromAxisAngle(joint.axis, angle));
      }
      for (const [side, offset] of [["Left", 0], ["Right", Math.PI]]) {
        const swing = Math.sin(phase + offset);
        pose(`${side} leg`, swing * THREE.MathUtils.lerp(0.5, 0.85, sprint) * moving);
        pose(`${side} knee`, Math.max(0, -swing) * THREE.MathUtils.lerp(0.65, 1.15, sprint) * moving);
        pose(`${side} ankle`, Math.max(0, swing) * 0.18 * moving);
        pose(`${side} arm`, -swing * THREE.MathUtils.lerp(0.4, 0.75, sprint) * moving);
        pose(`${side} elbow`, -(0.12 + sprint * 0.55) * moving);
      }
      visual.position.y = -1.6 + Math.abs(Math.sin(phase)) * 0.035 * moving;
    },
    dispose() {
      disposed = true;
      removePlaceholder();
      // Geometry/materials belong to the common template, bone textures don't.
      const skeletons = new Set();
      model?.traverse((node) => { if (node.skeleton) skeletons.add(node.skeleton); });
      for (const skeleton of skeletons) skeleton.dispose();
      root.clear();
      joints = {};
      model = null;
    },
  };
  return root;
}
