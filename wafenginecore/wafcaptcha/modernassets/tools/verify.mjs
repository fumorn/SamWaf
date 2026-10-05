// Offline worker/WASM/fallback smoke tests; starts no HTTP server.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { createHash, webcrypto } from 'node:crypto';
import { deflateRawSync } from 'node:zlib';
import { runInNewContext } from 'node:vm';
const root = new URL('../', import.meta.url);
const read = name => readFileSync(new URL(name, root));
const sha = data => createHash('sha256').update(data).digest('hex');
const manifest = JSON.parse(read('manifest.json'));
for (const [name, hash] of Object.entries(manifest.files)) assert.equal(sha(read(name)), hash, name);
assert.equal(sha(read('vendor/pako-2.1.0.tgz')), manifest.pako.sha256, 'pako archive');
assert.equal(sha(read(manifest.hashwx.sourceArchive)), manifest.hashwx.sha256, 'HashWX corresponding source archive');
const worker = read('static/worker.js').toString();
const shaModule = await WebAssembly.compile(read('static/cap_wasm_bg.wasm'));
const hashwxModule = await WebAssembly.compile(read('static/hashwx.wasm'));
async function solve(data, wasm = WebAssembly, crypto = webcrypto) {
  const messages = [];
  const self = { CAP_SILENT: true, postMessage: msg => messages.push(msg) };
  const sandbox = { self, WebAssembly: wasm, TextEncoder, performance, crypto, console, setTimeout, clearTimeout, MessageChannel: undefined };
  runInNewContext(worker, sandbox, { filename: 'worker.js' });
  await self.onmessage({ data });
  const final = messages.find(m => m.found === true || m.error);
  assert(final, 'worker must emit final result');
  return final;
}
const salt = 'samwaf-modern-sha256';
const shaResult = await solve({ salt, target: '00a', wasmModule: shaModule });
assert.equal(shaResult.found, true);
assert(sha(salt + shaResult.nonce).startsWith('00a'), 'odd-nibble SHA256 proof');
const jsOnly = await solve({ salt, target: '0' }, {});
assert(sha(salt + jsOnly.nonce).startsWith('0'), 'SHA fallback proof');
for (const longSalt of ['a'.repeat(36), 'b'.repeat(55), 'c'.repeat(64), 'd'.repeat(512), '验'.repeat(20)]) {
  for (const crypto of [webcrypto, {}]) {
    const result = await solve({ salt: longSalt, target: '0a', wasmModule: shaModule }, WebAssembly, crypto);
    assert.equal(result.found, true, 'long salt must not enter single-block WASM');
    assert(sha(longSalt + result.nonce).startsWith('0a'), 'multi-block/UTF8 SHA fallback proof');
  }
}
const rsw = await solve({ kind: 'rsw', N: 'ca1', x: '2', t: 100 });
let y = 2n;
for (let i = 0; i < 100; i++) y = y * y % 3233n;
assert.equal(rsw.y, y.toString(16), 'RSW modular squaring proof');
const c = '12'.repeat(32);
const hashwx = await solve({ kind: 'hashwx', c, d: 10, n: 256, workerIndex: 0, workerCount: 1, hashwxModule });
assert.equal(hashwx.found, true);
// Independently verify using the WASM interpreter, not the worker's compiled side-module.
const hx = new WebAssembly.Instance(hashwxModule, {}).exports;
hx._initialize?.();
const ctx = hx.hashwx_alloc(0);
assert(ctx > 0);
const nonce = BigInt(hashwx.nonce);
const seed = Buffer.alloc(40);
Buffer.from(c, 'hex').copy(seed);
seed.writeBigUInt64LE(nonce / 256n, 32);
const ptr = hx.hashwx_seed(ctx);
new Uint8Array(hx.memory.buffer, ptr, 32).set(createHash('sha256').update(seed).digest());
hx.hashwx_make(ctx, ptr);
const hash = BigInt.asUintN(64, hx.hashwx_exec(ctx, nonce));
assert(hash <= ((1n << 64n) - 1n) / 10n, 'HashWX compiled worker proof validates in interpreter');
const pakoSandbox = {};
runInNewContext(read('static/pako_inflate.min.js').toString(), pakoSandbox);
const instrumentation = 'parent.postMessage({type:"cap:instr",result:{ok:true}},"*");';
assert.equal(Buffer.from(pakoSandbox.pako.inflateRaw(deflateRawSync(instrumentation))).toString(), instrumentation, 'offline instrumentation deflate-raw fallback');
assert(!read('static/cap.js').toString().includes('cdn.jsdelivr.net'));
console.log('PASS: manifest SHA256, SHA256 WASM odd-nibble proof, JS fallback, RSW, HashWX compiled proof/interpreter verification, pako raw-deflate fallback.');
