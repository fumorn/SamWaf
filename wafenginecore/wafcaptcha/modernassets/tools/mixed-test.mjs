// Offline real-thread regression for upstream mixed-protocol scheduling.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { Worker as Thread } from 'node:worker_threads';
import { runInNewContext } from 'node:vm';
import { createHash } from 'node:crypto';
const read = name => readFileSync(new URL('../static/' + name, import.meta.url));
const cap = read('cap.js').toString();
const source = read('worker.js').toString();
const wasmModule = await WebAssembly.compile(read('cap_wasm_bg.wasm'));
const hashwxModule = await WebAssembly.compile(read('hashwx.wasm'));
const live = new Set();
class Worker {
  constructor() {
    this.listeners = new Map();
    this.thread = new Thread(`const { parentPort } = require('node:worker_threads');
      global.self = { CAP_SILENT: true, postMessage: msg => parentPort.postMessage(msg) };
      ${source}
      parentPort.on('message', data => { void self.onmessage({data}); });`, { eval: true });
    live.add(this);
    this.thread.on('message', data => { for (const fn of [...(this.listeners.get('message') || [])]) fn({ data }); });
    this.thread.on('error', error => { for (const fn of this.listeners.get('error') || []) fn(error); });
  }
  postMessage(data) { this.thread.postMessage(data); }
  addEventListener(name, fn) { if (!this.listeners.has(name)) this.listeners.set(name, new Set()); this.listeners.get(name).add(fn); }
  removeEventListener(name, fn) { this.listeners.get(name)?.delete(fn); }
  terminate() { live.delete(this); void this.thread.terminate(); }
}
const poolSource = cap.slice(cap.indexOf('  class WorkerPool {'), cap.indexOf('  class CapWidget extends'));
const methodSource = cap.slice(cap.indexOf('    async solveChallengesV2('), cap.indexOf('    async solveChallenges(challenges, signal)')).replaceAll('this.#workersCount', 'this.workersCount');
const sandbox = {
  Worker, WebAssembly, setTimeout, clearTimeout, Promise,
  _getSharedWorkerUrl: () => 'local-worker', WORKER_STOP_GRACE_MS: 250, HASHWX_SETTLE_MS: 1000,
  MAX_HASHWX_DIFFICULTY: 1e9, MAX_HASHWX_NONCES_PER_HASH: 1048576,
  _err: (code, message) => Object.assign(new Error(message), { code }),
  log: { debug: (...args) => { if (process.env.CAP_DEBUG) console.log(...args); }, warn: console.warn, error: console.error }, T: x => x,
  getWasmModule: async () => wasmModule, getHashwxModule: async () => hashwxModule,
  firstFulfilled: tasks => Promise.any(tasks), raceAbort: promise => promise,
  runInstrumentationChallenge: async () => ({ browser: true }),
};
const solver = runInNewContext(`${poolSource}\n({workersCount:4,dispatchEvent(){},${methodSource}})`, sandbox);
const challenges = [
  { protocol: 'sha256-pow', payload: { salt: 'c'.repeat(64), target: '0' } },
  { protocol: 'hashwx', payload: { c: '12'.repeat(32), d: 8, n: 256 } },
  { protocol: 'hashwx', payload: { c: '34'.repeat(32), d: 8, n: 256 } },
  { protocol: 'rsw', payload: { N: 'f'.repeat(512), x: '9'.repeat(512), t: 10000 } },
  { protocol: 'instrumentation', payload: { blob: 'offline-stub' } },
];
const deadline = setTimeout(() => { for (const w of live) w.terminate(); console.error('Mixed-thread solve timeout'); process.exit(1); }, 15000);
try {
  for (const count of [1, 2, 4]) {
    solver.workersCount = count;
    for (let i = 0; i < 5; i++) {
      const solutions = await solver.solveChallengesV2(challenges);
      assert.equal(solutions.length, challenges.length);
      assert(createHash('sha256').update(challenges[0].payload.salt + solutions[0].nonce).digest('hex').startsWith('0'));
      assert.equal(typeof solutions[1].nonce, 'string');
      assert.equal(typeof solutions[2].nonce, 'string');
      let y = BigInt('0x' + challenges[3].payload.x);
      const modulus = BigInt('0x' + challenges[3].payload.N);
      for (let n = 0; n < 10000; n++) y = y * y % modulus;
      assert.equal(solutions[3].y, y.toString(16));
      assert.equal(solutions[4].instr.browser, true);
    }
  }
  console.log('PASS: 15 real-thread mixed SHA256 + 2 HashWX + RSW + instrumentation-stub solves, worker counts 1/2/4.');
} finally { clearTimeout(deadline); for (const w of live) w.terminate(); }
