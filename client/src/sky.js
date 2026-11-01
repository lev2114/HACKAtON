import * as THREE from "three";

// Created once for the entire session, outside every room/chunk lifecycle.
// All textures are generated locally; no downloads or existing assets needed.
export function createCosmicSky() {
  const scene = new THREE.Scene();
  scene.background = new THREE.Color(0x02040c);
  const camera = new THREE.PerspectiveCamera(70, 1, 0.1, 2000);

  const dome = new THREE.Mesh(
    new THREE.SphereGeometry(1400, 32, 16),
    new THREE.MeshBasicMaterial({ map: makeNebula(), side: THREE.BackSide, depthWrite: false })
  );
  scene.add(dome);
  addStars(scene);

  scene.add(new THREE.AmbientLight(0x9cbaff, 0.28));
  const sunlight = new THREE.DirectionalLight(0xfff1dc, 2.6);
  sunlight.position.set(-250, 180, 500);
  scene.add(sunlight);

  const earth = new THREE.Mesh(
    new THREE.SphereGeometry(91, 64, 48),
    new THREE.MeshPhongMaterial({ map: makeEarth(), specular: 0x234970, shininess: 18 })
  );
  earth.position.set(-165, 65, -490);
  earth.rotation.set(0.08, -1.1, -0.12);
  scene.add(earth);

  const clouds = new THREE.Mesh(
    new THREE.SphereGeometry(91.6, 64, 48),
    new THREE.MeshPhongMaterial({ map: makeClouds(), transparent: true, opacity: 0.86, depthWrite: false })
  );
  earth.add(clouds);
  earth.add(new THREE.Mesh(
    new THREE.SphereGeometry(94, 64, 48),
    new THREE.ShaderMaterial({
      transparent: true,
      depthWrite: false,
      blending: THREE.AdditiveBlending,
      toneMapped: false,
      vertexShader: `
        varying vec3 surfaceNormal;
        varying vec3 viewDirection;
        void main() {
          vec4 viewPosition = modelViewMatrix * vec4(position, 1.0);
          surfaceNormal = normalize(normalMatrix * normal);
          viewDirection = normalize(-viewPosition.xyz);
          gl_Position = projectionMatrix * viewPosition;
        }
      `,
      fragmentShader: `
        varying vec3 surfaceNormal;
        varying vec3 viewDirection;
        void main() {
          float rim = pow(1.0 - max(dot(normalize(surfaceNormal), normalize(viewDirection)), 0.0), 3.0);
          gl_FragColor = vec4(0.12, 0.42, 1.0, rim * 0.72);
          #include <colorspace_fragment>
        }
      `,
    })
  ));

  const moonTexture = makeMoon();
  const moon = new THREE.Mesh(
    new THREE.SphereGeometry(25, 48, 32),
    new THREE.MeshPhongMaterial({ map: moonTexture, bumpMap: moonTexture, bumpScale: 0.5, shininess: 0 })
  );
  moon.position.set(105, 105, -420);
  scene.add(moon);

  return {
    render(renderer, world, playerCamera) {
      // Orientation follows looking around, but position NEVER follows room
      // coordinates. Earth, Moon and stars therefore cannot jump on teleport.
      if (camera.aspect !== playerCamera.aspect || camera.fov !== playerCamera.fov || camera.zoom !== playerCamera.zoom) {
        camera.aspect = playerCamera.aspect;
        camera.fov = playerCamera.fov;
        camera.zoom = playerCamera.zoom;
        camera.updateProjectionMatrix();
      }
      playerCamera.getWorldQuaternion(camera.quaternion);
      const autoClear = renderer.autoClear;
      renderer.autoClear = false;
      try {
        renderer.clear();
        renderer.render(scene, camera);
        renderer.clearDepth();
        renderer.render(world, playerCamera);
      } finally {
        renderer.autoClear = autoClear;
      }
    },
  };
}

function seededRandom(seed) {
  return () => {
    seed = (Math.imul(seed, 1664525) + 1013904223) >>> 0;
    return seed / 4294967296;
  };
}

function canvasTexture(width, height, draw) {
  const canvas = document.createElement("canvas");
  canvas.width = width;
  canvas.height = height;
  draw(canvas.getContext("2d"), width, height);
  const texture = new THREE.CanvasTexture(canvas);
  texture.colorSpace = THREE.SRGBColorSpace;
  texture.wrapS = THREE.RepeatWrapping;
  return texture;
}

function noise(x, y, z) {
  const ix = Math.floor(x), iy = Math.floor(y), iz = Math.floor(z);
  const smooth = (v) => v * v * (3 - 2 * v);
  const u = smooth(x - ix), v = smooth(y - iy), w = smooth(z - iz);
  const hash = (a, b, c) => {
    let n = Math.imul(a, 374761393) ^ Math.imul(b, 668265263) ^ Math.imul(c, 2147483647);
    n = Math.imul(n ^ (n >>> 13), 1274126177);
    return ((n ^ (n >>> 16)) >>> 0) / 4294967295;
  };
  const mix = (a, b, t) => a + (b - a) * t;
  return mix(
    mix(mix(hash(ix, iy, iz), hash(ix + 1, iy, iz), u), mix(hash(ix, iy + 1, iz), hash(ix + 1, iy + 1, iz), u), v),
    mix(mix(hash(ix, iy, iz + 1), hash(ix + 1, iy, iz + 1), u), mix(hash(ix, iy + 1, iz + 1), hash(ix + 1, iy + 1, iz + 1), u), v),
    w
  );
}

function cloudsAt(x, y, z) {
  return noise(x, y, z) * 0.55 + noise(x * 2.1, y * 2.1, z * 2.1) * 0.3 + noise(x * 4.3, y * 4.3, z * 4.3) * 0.15;
}

// Sample directions on a sphere, keeping procedural textures seamless at +/-180°.
function sphericalTexture(paint) {
  return canvasTexture(512, 256, (ctx, width, height) => {
    const pixels = ctx.createImageData(width, height);
    for (let row = 0; row < height; row++) {
      const latitude = (0.5 - row / (height - 1)) * Math.PI;
      const ring = Math.cos(latitude);
      for (let col = 0; col < width; col++) {
        const longitude = (col / (width - 1) - 0.5) * Math.PI * 2;
        const rgba = paint(ring * Math.cos(longitude), Math.sin(latitude), ring * Math.sin(longitude));
        const offset = (row * width + col) * 4;
        pixels.data.set(rgba, offset);
      }
    }
    ctx.putImageData(pixels, 0, 0);
  });
}

function makeNebula() {
  return sphericalTexture((x, y, z) => {
    const mist = cloudsAt(x * 3 + 12, y * 3 + 4, z * 3 + 8);
    const band = Math.exp(-Math.pow((y + x * 0.32 - z * 0.14) * 3.5, 2));
    const glow = Math.pow(mist, 2.2) * band;
    return [3 + glow * 26, 5 + glow * 24, 13 + glow * 63, 255];
  });
}

function addStars(scene) {
  const random = seededRandom(1701);
  const sprite = canvasTexture(32, 32, (ctx) => {
    const halo = ctx.createRadialGradient(16, 16, 0, 16, 16, 16);
    halo.addColorStop(0, "white");
    halo.addColorStop(0.15, "rgba(255,255,255,0.95)");
    halo.addColorStop(0.45, "rgba(255,255,255,0.25)");
    halo.addColorStop(1, "rgba(255,255,255,0)");
    ctx.fillStyle = halo;
    ctx.fillRect(0, 0, 32, 32);
  });
  for (const [count, size, opacity] of [[2600, 1.8, 0.68], [650, 3, 0.85], [90, 5, 1]]) {
    const positions = [], colors = [];
    for (let i = 0; i < count; i++) {
      const y = random() * 2 - 1;
      const angle = random() * Math.PI * 2;
      const ring = Math.sqrt(1 - y * y);
      positions.push(ring * Math.cos(angle) * 1000, y * 1000, ring * Math.sin(angle) * 1000);
      const color = new THREE.Color(random() < 0.2 ? 0xffdfb0 : random() < 0.35 ? 0xadcaff : 0xffffff);
      colors.push(color.r, color.g, color.b);
    }
    const geometry = new THREE.BufferGeometry();
    geometry.setAttribute("position", new THREE.Float32BufferAttribute(positions, 3));
    geometry.setAttribute("color", new THREE.Float32BufferAttribute(colors, 3));
    scene.add(new THREE.Points(geometry, new THREE.PointsMaterial({
      map: sprite, size, opacity, vertexColors: true, sizeAttenuation: false,
      transparent: true, depthWrite: false,
    })));
  }
}

function makeEarth() {
  return canvasTexture(1024, 512, (ctx, width, height) => {
    ctx.fillStyle = "#0b3d80";
    ctx.fillRect(0, 0, width, height);
    // Simplified original coastlines, drawn in longitude/latitude coordinates.
    const continents = [
      [[-168,70],[-145,72],[-125,62],[-110,70],[-82,75],[-54,53],[-64,46],[-81,26],[-86,20],[-77,8],[-91,17],[-105,22],[-118,33],[-126,49],[-147,59],[-168,60]],
      [[-81,12],[-65,10],[-48,-2],[-35,-8],[-41,-23],[-54,-36],[-67,-55],[-74,-46],[-77,-20],[-81,-4]],
      [[-52,83],[-25,76],[-43,60],[-58,65]],
      [[-10,36],[-10,58],[8,72],[30,70],[49,57],[70,72],[108,76],[150,65],[179,65],[168,50],[145,45],[132,33],[120,23],[108,6],[101,13],[96,22],[80,7],[70,25],[53,29],[44,12],[34,29],[28,41],[15,36]],
      [[-17,35],[10,37],[33,30],[43,12],[51,10],[42,-12],[32,-32],[18,-35],[10,-17],[-1,4],[-16,15]],
      [[113,-22],[123,-14],[137,-12],[145,-16],[154,-28],[146,-39],[130,-34],[115,-35]],
      [[48,-13],[51,-17],[48,-26],[44,-23]],
      [[130,31],[142,45],[145,41],[137,32]],
      [[-9,50],[-2,59],[1,53]],
      [[166,-35],[179,-38],[172,-47],[165,-45]],
      [[95,5],[108,-6],[119,-8],[112,1]],
    ];
    ctx.beginPath();
    for (const outline of continents) {
      outline.forEach(([lon, lat], i) => {
        const x = (lon + 180) / 360 * width, y = (90 - lat) / 180 * height;
        if (i === 0) ctx.moveTo(x, y); else ctx.lineTo(x, y);
      });
      ctx.closePath();
    }
    ctx.save();
    ctx.clip();
    const land = ctx.createLinearGradient(0, 100, 0, 410);
    land.addColorStop(0, "#88956c");
    land.addColorStop(0.4, "#537853");
    land.addColorStop(0.55, "#365e42");
    land.addColorStop(1, "#92926b");
    ctx.fillStyle = land;
    ctx.fillRect(0, 0, width, height);
    ctx.fillStyle = "#b6a17a";
    ctx.beginPath();
    ctx.ellipse(555, 190, 74, 23, -0.08, 0, Math.PI * 2);
    ctx.ellipse(890, 335, 48, 30, 0, 0, Math.PI * 2);
    ctx.fill();
    ctx.restore();
    const random = seededRandom(42);
    ctx.fillStyle = "#e6f1ee";
    for (const pole of [0, height]) {
      ctx.beginPath();
      ctx.moveTo(0, pole);
      for (let x = 0; x <= width; x += 8) {
        ctx.lineTo(x, pole === 0 ? 10 + random() * 13 : height - 18 - random() * 12);
      }
      ctx.lineTo(width, pole);
      ctx.closePath();
      ctx.fill();
    }
  });
}

function makeClouds() {
  return sphericalTexture((x, y, z) => {
    const density = cloudsAt(x * 7 + 5, y * 7 + 12, z * 7 + 2);
    const alpha = THREE.MathUtils.smoothstep(density, 0.48, 0.72) * 220;
    return [245, 250, 255, alpha];
  });
}

function makeMoon() {
  return canvasTexture(512, 256, (ctx, width, height) => {
    const random = seededRandom(1969);
    const pixels = ctx.createImageData(width, height);
    for (let i = 0; i < pixels.data.length; i += 4) {
      const shade = 156 + random() * 24;
      pixels.data.set([shade, shade, shade * 0.97, 255], i);
    }
    ctx.putImageData(pixels, 0, 0);
    for (let i = 0; i < 160; i++) {
      const x = random() * width, y = random() * height;
      const radius = 2 + Math.pow(random(), 3) * 28;
      // Repeat edge craters so the lunar texture wraps without a hard seam.
      for (const offset of [-width, 0, width]) {
        const crater = ctx.createRadialGradient(x + offset, y, radius * 0.4, x + offset, y, radius);
        crater.addColorStop(0, "rgba(55,56,61,0.42)");
        crater.addColorStop(0.7, "rgba(78,79,84,0.32)");
        crater.addColorStop(0.84, "rgba(215,213,201,0.45)");
        crater.addColorStop(1, "rgba(160,160,160,0)");
        ctx.fillStyle = crater;
        ctx.fillRect(x + offset - radius, y - radius, radius * 2, radius * 2);
      }
    }
  });
}
