// Deterministic local-only Cap assembly. Node is a build-time tool, never a server dependency.
import { readFileSync, writeFileSync, mkdirSync, readdirSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';

const root = resolve(dirname(fileURLToPath(import.meta.url)), '..');
const commit = 'af624b9f15cb03f1f8194fb4c9f262ac34984923';
const upstream = process.argv[2];
const sha = data => createHash('sha256').update(data).digest('hex');
const pins = {
  'widget/src/src/cap.js': 'f7b630ecc3142437d04d4046ba74c1218f4b3b97a984e69d19c5bc06717a07eb',
  'widget/src/src/cap.css': '18ca844acc0d2c242bdd8840a408e26cfed083d11fb86efc48875ff6363eb279',
  'widget/src/src/worker.js': 'ee8e77cb247ed476451bd508c27206c8a09f7682e613b92d1ba17bb9bcd6fd89',
  'widget/src/src/i18n/translations.js': '5d98b4209dd6dede2a427188ab817a37274e19b6ed6b92d2c5a6e61d26456bda',
  'wasm/src/browser/cap_wasm_bg.wasm': 'e4f3c00246a775193661f9277ca1288cd310a6514de166ecc2176ccd26fb06a9',
  'wasm/src/browser/hashwx.wasm': 'b1a0dbb3ef444d3c7069e0a5e0a0273ffa4cf8fef62cbbe43761c02f7cd6aff5',
  'LICENSE': '69acdeead2435cb91bd0d94aaadc8ee2d0fa7e30a789cdeeb9aa2e0d4a986318',
  'core/vendor/hashwx-LICENSE.txt': 'e3a994d82e644b03a792a930f574002658412f62407f5fee083f2555c5f23118',
};
const pakoArchive = 'vendor/pako-2.1.0.tgz';
const pakoHash = '49fedc8866b4abfc8e71dc7fe75ad4ef1ff1ac9601b0642cff88ee5bf2338709';
const output = (name, data) => writeFileSync(join(root, name), data);

if (upstream && upstream !== '--manifest-only') {
  const sources = {};
  for (const [name, hash] of Object.entries(pins)) {
    const data = readFileSync(join(resolve(upstream), name));
    if (sha(data) !== hash) throw new Error(`Pinned source mismatch: ${name}`);
    sources[name] = data;
  }
  if (sha(readFileSync(join(root, pakoArchive))) !== pakoHash) throw new Error('pako archive mismatch');
  mkdirSync(join(root, 'static'), { recursive: true });
  mkdirSync(join(root, 'licenses'), { recursive: true });
  const i18n = await import('data:text/javascript;base64,' + sources['widget/src/src/i18n/translations.js'].toString('base64'));
  const rows = Object.fromEntries(i18n.shipped.map(code => [code, i18n.shippedKeys.map(key => i18n.translations[code][i18n.keys.indexOf(key)]).join('/')]));
  let main = sources['widget/src/src/cap.js'].toString();
  const replace = (before, after) => {
    if (main.split(before).length !== 2) throw new Error('Patch anchor not unique: ' + before);
    main = main.replace(before, () => after);
  };
  replace('  const WASM_VERSION = "0.0.8";', `  // SamWaf modification: resolve every resource beside this local script, never a CDN.
  const ASSET_BASE = new URL(".", document.currentScript.src).href;`);
  replace('"https://cdn.jsdelivr.net/npm/pako@2.1.0/dist/pako_inflate.min.js"', 'new URL("pako_inflate.min.js", ASSET_BASE).href');
  replace('`https://cdn.jsdelivr.net/npm/@cap.js/wasm@${WASM_VERSION}/browser/cap_wasm_bg.wasm`', 'new URL("cap_wasm_bg.wasm", ASSET_BASE).href');
  replace('`https://cdn.jsdelivr.net/npm/@cap.js/wasm@${WASM_VERSION}/browser/hashwx.wasm`', 'new URL("hashwx.wasm", ASSET_BASE).href');
  replace('URL.createObjectURL(\n      new Blob([%%workerScript%%], { type: "application/javascript" }),\n    )', 'new URL("worker.js", ASSET_BASE).href');
  // Local diagnostics are opt-in, omit secrets/payloads and identify stalled phases.
  replace('const compressed = b64ToUint8(instrBytes);', 'log.debug(T("instr"), "inflate start");\n    const compressed = b64ToUint8(instrBytes);');
  replace('const scriptText = new TextDecoder().decode(await _inflateRaw(compressed));', 'const scriptText = new TextDecoder().decode(await _inflateRaw(compressed));\n    log.debug(T("instr"), "inflate complete; starting sandbox");');
  replace('solutions[idx] = { nonce: Number(nonce) };', 'log.debug(T("solve"), `sha256 #${idx}: complete`);\n                solutions[idx] = { nonce: Number(nonce) };');
  replace('solutions[idx] = { y: data.y };', 'log.debug(T("solve"), `rsw #${idx}: complete`);\n                solutions[idx] = { y: data.y };');
  replace('if (out?.__timeout) solutions[idx] = { timeout: true };', 'log.debug(T("solve"), `instrumentation #${idx}: complete`);\n              if (out?.__timeout) solutions[idx] = { timeout: true };');
  replace('%%i18nKeys%%', i18n.shippedKeys.join(','));
  replace('%%i18nData%%', JSON.stringify(rows));
  replace('%%capCSS%%', sources['widget/src/src/cap.css'].toString().replaceAll('\\', '\\\\').replaceAll('`', '\\`').replaceAll('${', '\\${'));
  if (/cdn\.|%%\w+%%/.test(main)) throw new Error('Unexpected CDN or unresolved build token');
  output('static/cap.js', `/*! Cap widget 0.1.58; Apache-2.0; upstream ${commit}.
 * Modified by SamWaf: deterministic unminified assembly; external local worker;
 * local-only WASM and pako defaults. See licenses/CAP.txt and README.md. */\n` + main);
  let worker = sources['widget/src/src/worker.js'].toString();
  const wasmGuard = '    const { salt, target, wasmModule } = data;';
  if (worker.split(wasmGuard).length !== 2) throw new Error('worker patch anchor mismatch');
  worker = worker.replace(wasmGuard, wasmGuard + `
    // SamWaf: WASM 0.0.8 only hashes a single SHA256 block (<56 bytes).
    // Format-2 salts may exceed that. Reserve all 20 decimal u64 digits;
    // otherwise the upstream WASM loops forever instead of finding a proof.
    if (new TextEncoder().encode(salt).length > 35) {
      return solveFallback({ salt, target });
    }`);
  // Reuse upstream's general multi-block SHA256 on non-secure HTTP where
  // crypto.subtle is unavailable; never load an external hashing dependency.
  worker = worker.replace('const hashBuffer = await crypto.subtle.digest("SHA-256", inputBytes);', `let hashBuffer;
          if (typeof crypto !== "undefined" && crypto.subtle) {
            hashBuffer = await crypto.subtle.digest("SHA-256", inputBytes);
          } else {
            const out = new Uint8Array(32);
            sha256Into(inputBytes, out);
            hashBuffer = out.buffer;
          }`);
  output('static/worker.js', `/*! Cap worker; Apache-2.0; upstream ${commit}.
 * Modified by SamWaf: guard single-block WASM input and use local SHA fallback
 * for long salts and browsers without crypto.subtle. */\n` + worker);
  for (const file of ['cap_wasm_bg.wasm', 'hashwx.wasm']) output('static/' + file, sources['wasm/src/browser/' + file]);
  output('licenses/CAP.txt', sources.LICENSE);
  output('licenses/HASHWX-LGPL-3.txt', sources['core/vendor/hashwx-LICENSE.txt']);
  output('static/pako_inflate.min.js', execFileSync('tar', ['-xOf', join(root, pakoArchive), 'package/dist/pako_inflate.min.js']));
  output('licenses/PAKO.txt', execFileSync('tar', ['-xOf', join(root, pakoArchive), 'package/LICENSE']));
} else if (upstream !== '--manifest-only') {
  throw new Error('Usage: node tools/generate.mjs /path/to/pinned/cap-source (or --manifest-only)');
}
const files = {};
for (const dir of ['static', 'licenses']) for (const name of readdirSync(join(root, dir)).sort()) {
  const path = `${dir}/${name}`;
  files[path] = sha(readFileSync(join(root, path)));
}
files['challenge.html'] = sha(readFileSync(join(root, 'challenge.html')));
output('manifest.json', JSON.stringify({ upstream: { repository: 'https://github.com/tiagorangel1/cap', commit, core: '0.1.3', widget: '0.1.58', wasm: '0.0.8' }, sources: pins, pako: { version: '2.1.0', url: 'https://registry.npmjs.org/pako/-/pako-2.1.0.tgz', sha256: pakoHash }, hashwx: { repository: 'https://github.com/tevador/hashwx', commit: '74b567a31276a4c5d7cb232a5b7639467f49f961', sourceArchive: 'vendor/hashwx-74b567a31276a4c5d7cb232a5b7639467f49f961.tar.gz', sha256: '14f9b95399abae1c4a3d2d1b92e1402c0cdaa870cc4f337fe960b93ce6416e99', license: 'LGPL-3.0' }, files }, null, 2) + '\n');
console.log(`Pinned local assets assembled; ${Object.keys(files).length} embedded file hashes recorded.`);
