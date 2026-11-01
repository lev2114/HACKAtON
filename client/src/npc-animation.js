import * as THREE from "three";

const HEIGHT = 1.7;
// Per-character size tweak by NPC id, on top of HEIGHT: Krosh's height
// includes his long ears, so at 1.7 m his body looked smaller than the rest.
const SIZE_BY_NPC = { krosh: 1.3 };
// Fractions of each model's full height, including hats/antlers/ears.
// Round bodies have no separate neck: keep the face, hat and body together.
const profiles = {
  krosh: { lateral: "z", shoulder: 0.47, armLow: 0.23, armHigh: 0.70, armInner: 0.24, armOuter: 0.32, hip: 0.14, ears: 0.72 },
  "krosh-classic": { shoulder: 0.28, armLow: 0.19, armHigh: 0.37, hip: 0.10, ears: 0.63 },
  kopatych: { shoulder: 0.55, armLow: 0.40, armHigh: 0.73, hip: 0.15 },
  losyash: { shoulder: 0.44, armLow: 0.30, armHigh: 0.56, hip: 0.13 },
  nyusha: { shoulder: 0.44, armLow: 0.30, armHigh: 0.56, hip: 0.14 },
  pin: { shoulder: 0.56, armLow: 0.10, armHigh: 0.66, hip: 0.15 },
  sovunya: { shoulder: 0.42, armLow: 0.26, armHigh: 0.55, hip: 0.13 },
};
const smooth = (a, b, x) => THREE.MathUtils.smoothstep(x, a, b);

// Called before attaching the imported scene to its world-space NPC holder.
// All changes are in memory; source GLBs and their authored rigs are preserved.
export function createNpcAnimation(source, npc) {
  source.updateMatrixWorld(true); // Also refresh SkinnedMesh.bindMatrixInverse.
  const bounds = new THREE.Box3().setFromObject(source);
  const size = bounds.getSize(new THREE.Vector3());
  const center = bounds.getCenter(new THREE.Vector3());
  const scale = (HEIGHT * (SIZE_BY_NPC[npc.id] ?? 1)) / (size.y || 1);
  const fitted = new THREE.Group();
  const bones = [];
  source.traverse((node) => { if (node.isBone) bones.push(node); });
  const kind = npc.model.split("/").pop().replace("-character.glb", "");
  let joints;

  if (bones.length) {
    // Fit outside the authored hierarchy so skin bind matrices, accessories
    // and independent skeletons (Yozhik's glasses) remain in the same space.
    fitted.scale.setScalar(scale);
    fitted.position.set(-center.x * scale, -1 - bounds.min.y * scale, -center.z * scale);
    fitted.add(source);
    fitted.updateMatrixWorld(true);
    joints = authoredJoints(bones, kind);
  } else {
    const normalization = new THREE.Matrix4().makeTranslation(
      -center.x * scale, -1 - bounds.min.y * scale, -center.z * scale
    ).multiply(new THREE.Matrix4().makeScale(scale, scale, scale));
    joints = addSkeleton(source, fitted, normalization, size.clone().multiplyScalar(scale), profiles[kind] || profiles.kopatych);
  }
  lowerArms(fitted, joints, kind);

  // Each room owns its animator; no global model/rig cache survives unloading.
  let time = 0, gesture = 0, stride = 0, walking = 0;
  const phase = [...npc.id].reduce((sum, ch) => sum + ch.charCodeAt(0), 0) % 31;
  const rotation = new THREE.Quaternion();
  const swingRotation = new THREE.Quaternion();
  // Rotate the centered fitted model, not its offset import hierarchy.
  // Krosh and Yozhik face +X in their source files; all actor poses face +Z.
  const visual = new THREE.Group();
  visual.rotation.y = kind === "krosh" || kind === "yozhik" ? -Math.PI / 2 : 0;
  visual.add(fitted);
  return {
    model: visual,
    update(dt, conversing = false, speed = 0) {
      time += dt;
      gesture = THREE.MathUtils.damp(gesture, conversing ? 1 : 0, 5, dt);
      walking = THREE.MathUtils.damp(walking, conversing ? 0 : Math.min(speed / 0.45, 1), 10, dt);
      stride += Math.min(speed, 0.8) * dt * Math.PI * 2 / 0.8;
      visual.position.y = Math.abs(Math.sin(stride)) * 0.025 * walking;
      const idle = time * 1.6 + phase;
      for (const joint of joints) {
        let angle;
        if (joint.part === "body") angle = Math.sin(idle * 0.65) * 0.012;
        else if (joint.part === "ear") angle = Math.sin(idle * 0.9 + joint.side) * 0.025;
        else if (joint.part === "leg") angle = Math.sin(stride + (joint.side > 0 ? Math.PI : 0)) * 0.30 * walking;
        else angle = (joint.neutral || 0) + Math.sin(idle + joint.side * 0.7) * 0.025 +
          gesture * joint.side * (0.06 + Math.sin(time * 3.2 + phase) * 0.025);
        for (const rest of joint.rests) {
          rotation.setFromAxisAngle(rest.axis, angle);
          if (rest.walkAxis) {
            const swing = Math.sin(stride + (joint.side > 0 ? Math.PI : 0)) * 0.22 * walking;
            rotation.premultiply(swingRotation.setFromAxisAngle(rest.walkAxis, -swing));
          }
          rest.bone.quaternion.copy(rest.quaternion).premultiply(rotation);
          // Kar-Karych's upper/lower wing bones are siblings, rather than a
          // chain. Rotate both around the shoulder to keep the wing connected.
          rest.bone.position.copy(rest.position).sub(rest.pivot).applyQuaternion(rotation).add(rest.pivot);
        }
      }
    },
  };
}

function lowerArms(model, joints, kind) {
  model.updateMatrixWorld(true);
  const lateral = kind === "krosh" || kind === "yozhik" ? "z" : "x";
  const axis = lateral === "x" ? new THREE.Vector3(0, 0, 1) : new THREE.Vector3(1, 0, 0);
  const vertex = new THREE.Vector3();
  for (const arm of joints.filter((entry) => entry.part === "arm")) {
    // Authored wings can have sibling upper/lower bones; authored hands can
    // have finger descendants. Include both when measuring the limb direction.
    const limb = new Set();
    for (const rest of arm.rests) rest.bone.traverse((bone) => { if (bone.isBone) limb.add(bone); });
    const center = new THREE.Vector3();
    let total = 0;
    model.traverse((mesh) => {
      if (!mesh.isSkinnedMesh) return;
      const indices = mesh.geometry.attributes.skinIndex;
      const weights = mesh.geometry.attributes.skinWeight;
      for (let i = 0; i < indices.count; i++) {
        let weight = 0;
        for (let j = 0; j < 4; j++) {
          if (limb.has(mesh.skeleton.bones[indices.getComponent(i, j)])) weight += weights.getComponent(i, j);
        }
        if (weight < 0.7) continue; // Keep the shoulder's torso blend out.
        mesh.getVertexPosition(i, vertex).applyMatrix4(mesh.matrixWorld);
        center.addScaledVector(vertex, weight);
        total += weight;
      }
    });
    if (!total) continue;
    const pivot = arm.rests[0].bone.getWorldPosition(new THREE.Vector3());
    const from = center.divideScalar(total).sub(pivot);
    // Aim downward in the character's lateral plane, slightly away from its
    // round body. Pin/Yozhik already point down, so they need little correction.
    const direction = new THREE.Vector3();
    direction[lateral] = from[lateral]; direction.y = from.y;
    if (direction.lengthSq() < 1e-8) continue;
    direction.normalize();
    const down = new THREE.Vector3(0, -1, 0);
    down[lateral] = arm.side * 0.20;
    down.normalize();
    arm.neutral = Math.atan2(axis.dot(new THREE.Vector3().crossVectors(direction, down)), direction.dot(down));
  }
}

function joint(part, side, bones, axis, walkAxis) {
  const pivot = bones[0].getWorldPosition(new THREE.Vector3());
  return { part, side, rests: bones.map((bone) => ({
    bone, quaternion: bone.quaternion.clone(), position: bone.position.clone(),
    pivot: bone.parent.worldToLocal(pivot.clone()),
    axis: axis.clone().applyQuaternion(bone.parent.getWorldQuaternion(new THREE.Quaternion()).invert()),
    walkAxis: walkAxis?.clone().applyQuaternion(bone.parent.getWorldQuaternion(new THREE.Quaternion()).invert()),
  })) };
}

function authoredJoints(bones, kind) {
  // GLTFLoader removes punctuation from bone names. Avoid fuzzy matches that
  // could accidentally select a facial control or a finger instead of an arm.
  const named = new Map(bones.map((bone) => [bone.name, bone]));
  const joints = [];
  if (kind === "karich") {
    for (const [side, sign] of [["left", -1], ["right", 1]]) {
      const upper = named.get(`hand_${side}_top_${side === "left" ? 1 : 0}`);
      const lower = named.get(`hand_${side}_bottom_${side === "left" ? 14 : 13}`);
      if (upper && lower) joints.push(joint("arm", sign, [upper, lower], new THREE.Vector3(0, 0, 1), new THREE.Vector3(1, 0, 0)));
      const thigh = named.get(side === "left" ? "leg_left_top_10" : "Bone005_9");
      const foot = named.get(`leg_${side}_bottom_${side === "left" ? 16 : 15}`);
      if (thigh && foot) joints.push(joint("leg", sign, [thigh, foot], new THREE.Vector3(1, 0, 0)));
    }
  } else if (kind === "yozhik") {
    for (const [name, sign] of [["Bone035_40", -1], ["Bone036_50", 1]]) {
      const bone = named.get(name);
      if (bone) joints.push(joint("arm", sign, [bone], new THREE.Vector3(1, 0, 0), new THREE.Vector3(0, 0, 1)));
    }
    for (const [name, sign] of [["Bone041_53", -1], ["Bone043_55", 1]]) {
      const bone = named.get(name);
      if (bone) joints.push(joint("leg", sign, [bone], new THREE.Vector3(0, 0, 1)));
    }
  }
  return joints;
}

function addSkeleton(source, model, normalization, size, profile) {
  const lateral = profile.lateral || "x";
  const width = size[lateral];
  const root = new THREE.Bone(); root.name = "NPC_root";
  model.add(root);
  const bones = [root];
  function addBone(name, parent, across, height) {
    const bone = new THREE.Bone(); bone.name = `NPC_${name}`;
    bone.position[lateral] = across;
    bone.position.y = height;
    parent.add(bone); bones.push(bone);
    return bone;
  }
  const bodyY = -1 + HEIGHT * 0.38;
  const body = addBone("body", root, 0, bodyY);
  const arms = [], legs = [], ears = [];
  for (const sign of [-1, 1]) {
    arms.push(addBone(`arm_${sign}`, body, sign * width * 0.27, -1 + HEIGHT * profile.shoulder - bodyY));
    legs.push(addBone(`leg_${sign}`, root, sign * width * 0.12, -1 + HEIGHT * profile.hip));
    if (profile.ears) ears.push(addBone(`ear_${sign}`, body, sign * width * 0.10, -1 + HEIGHT * profile.ears - bodyY));
  }
  model.updateMatrixWorld(true);
  const skeleton = new THREE.Skeleton(bones);
  const originals = new Set();
  const vertex = new THREE.Vector3();
  source.traverse((mesh) => {
    if (!mesh.isMesh) return;
    originals.add(mesh.geometry);
    const geometry = mesh.geometry.clone();
    geometry.applyMatrix4(new THREE.Matrix4().multiplyMatrices(normalization, mesh.matrixWorld));
    const positions = geometry.attributes.position;
    const indices = new Uint16Array(positions.count * 4);
    const weights = new Float32Array(positions.count * 4);
    for (let i = 0; i < positions.count; i++) {
      vertex.fromBufferAttribute(positions, i);
      const y = (vertex.y + 1) / HEIGHT;
      const across = Math.abs(vertex[lateral]) / width;
      const side = vertex[lateral] < 0 ? 0 : 1;
      // Feather attachments into the body; the central face remains rigid.
      const arm = smooth(profile.armInner ?? 0.29, profile.armOuter ?? 0.42, across) * smooth(profile.armLow - 0.06, profile.armLow + 0.04, y) *
        (1 - smooth(profile.armHigh - 0.04, profile.armHigh + 0.03, y));
      const leg = 1 - smooth(profile.hip, profile.hip + 0.09, y);
      const ear = profile.ears ? smooth(profile.ears, profile.ears + 0.10, y) : 0;
      const remainder = 1 - leg;
      indices.set([bones.indexOf(body), bones.indexOf(arms[side]), bones.indexOf(legs[side]), ears.length ? bones.indexOf(ears[side]) : 0], i * 4);
      weights.set([remainder * (1 - arm) * (1 - ear), remainder * arm * (1 - ear), leg, remainder * ear], i * 4);
    }
    geometry.setAttribute("skinIndex", new THREE.Uint16BufferAttribute(indices, 4));
    geometry.setAttribute("skinWeight", new THREE.Float32BufferAttribute(weights, 4));
    const skinned = new THREE.SkinnedMesh(geometry, mesh.material);
    skinned.name = `${mesh.name}_rigged`;
    // Small NPC count and idle motion: avoid a stale rest-pose bounding sphere
    // clipping a wing/ear after a gesture, without per-frame vertex scans.
    skinned.frustumCulled = false;
    model.add(skinned);
    skinned.bind(skeleton, new THREE.Matrix4());
  });
  for (const geometry of originals) geometry.dispose();
  model.updateMatrixWorld(true);
  const axis = lateral === "x" ? new THREE.Vector3(0, 0, 1) : new THREE.Vector3(1, 0, 0);
  const walkAxis = lateral === "x" ? new THREE.Vector3(1, 0, 0) : new THREE.Vector3(0, 0, 1);
  return [joint("body", 0, [body], axis),
    ...arms.map((bone, i) => joint("arm", i ? 1 : -1, [bone], axis, walkAxis)),
    ...legs.map((bone, i) => joint("leg", i ? 1 : -1, [bone], walkAxis)),
    ...ears.map((bone, i) => joint("ear", i ? 1 : -1, [bone], axis))];
}
