import * as THREE from "three";

// Small procedural prop: the imported character assets remain untouched.
export function createApple() {
  const group = new THREE.Group();
  const fruit = new THREE.Mesh(new THREE.SphereGeometry(0.09, 16, 12),
    new THREE.MeshStandardMaterial({ color: 0xe33d39, roughness: 0.4 }));
  fruit.scale.set(1, 0.90, 1); fruit.position.y = 0.08;
  const stem = new THREE.Mesh(new THREE.CylinderGeometry(0.008, 0.01, 0.045, 6),
    new THREE.MeshStandardMaterial({ color: 0x71452b }));
  stem.position.set(0, 0.175, 0); stem.rotation.z = -0.2;
  const leaf = new THREE.Mesh(new THREE.SphereGeometry(0.025, 8, 6),
    new THREE.MeshStandardMaterial({ color: 0x59aa45 }));
  leaf.scale.set(1, 0.15, 0.5); leaf.rotation.z = 0.35; leaf.position.set(0.025, 0.18, 0);
  group.add(fruit, stem, leaf);
  return group;
}

export function disposeItem(group) {
  group.traverse((node) => { node.geometry?.dispose(); node.material?.dispose(); });
}
