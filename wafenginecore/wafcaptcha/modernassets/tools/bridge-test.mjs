// Deterministic bridge tests for the original-style challenge page; no server or browser launch.
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { runInNewContext } from 'node:vm';
const source = readFileSync(new URL('../static/app.js', import.meta.url), 'utf8');

class Element {
  constructor(tag = 'div') {
    this.tagName = tag; this.style = {}; this.listeners = {}; this.children = [];
    this.attributes = {}; this.dataset = {}; this.classList = { add(...a){this._c=new Set([...(this._c||[]),...a])}, remove(){} };
    this.parentNode = null; this.hidden = false;
  }
  addEventListener(name, callback) { (this.listeners[name] ||= []).push(callback); }
  emit(name, detail) { for (const cb of this.listeners[name] || []) cb({ detail }); }
  setAttribute(name, value) { this.attributes[name] = value; if (name.startsWith('data-')) this.dataset[name.slice(5).replace(/-([a-z])/g,(_,c)=>c.toUpperCase())] = value; }
  getAttribute(name) { return this.attributes[name]; }
  appendChild(child) { child.parentNode = this; this.children.push(child); return child; }
  remove() { if (this.parentNode) { const i = this.parentNode.children.indexOf(this); if (i >= 0) this.parentNode.children.splice(i, 1); this.parentNode = null; } }
  querySelector() { return null; }
  set textContent(v) { this._text = v; } get textContent() { return this._text || ''; }
  set className(v) { this.attributes.class = v; this.classList._c = new Set(String(v).split(/\s+/)); } get className() { return this.attributes.class || ''; }
}
const tick = () => new Promise(resolve => setImmediate(resolve));

async function setup(method = 'GET', response = { success: true }, ok = true) {
  const els = Object.fromEntries(['samwaf-cap-page','page-title','lang-switch','info-title','info-text','cap'].map(id => [id, new Element()]));
  const body = els['samwaf-cap-page'];
  body.dataset = { prefix: '/custom/cap', requestMethod: method, titleZh: '<img onerror=x>', titleEn: 'Verify', textZh: '配置中文', textEn: 'Configured EN' };
  body.attributes = { id: 'samwaf-cap-page' };
  els.cap.setAttribute('data-cap-api-endpoint', '/custom/cap/v2/');
  const container = new Element(); container.attributes = { class: 'container' };
  const head = new Element();
  const created = [];
  const document = {
    getElementById: id => els[id] || null,
    querySelector: sel => sel === '.container' ? container : sel === '.toast' ? created.find(c => c.classList._c?.has?.('toast')) || null : (sel === '#cap' ? els.cap : null),
    createElement: tag => { const e = new Element(tag); created.push(e); return e; },
    body, head, cookie: '',
  };
  const location = { href: 'https://site.example/form?q=1', origin: 'https://site.example', replace(url) { this.navigation = url; } };
  const calls = []; const timers = new Map();
  const customElements = { get: () => false };
  const window = { customElements, Worker: class {}, WebAssembly, fetch: true, location };
  const fetch = async (url, options = {}) => { calls.push({ url, options }); return { ok, status: ok ? 200 : 403, json: async () => response }; };
  const setTimeout = (fn, ms) => { const id = {}; timers.set(id, { fn, ms }); return id; };
  const clearTimeout = id => timers.delete(id);
  runInNewContext(source, { window, document, navigator: { language: 'en', hardwareConcurrency: 8 }, location, customElements, URL, AbortController, Response, fetch, setTimeout, clearTimeout, console, Promise });
  await tick(); await tick();
  return { els, body, container, document, created, calls, timers, location, window, widget: els.cap };
}

// Resource overrides and endpoint guard.
{
  const ctx = await setup();
  assert.equal(ctx.widget.getAttribute('data-cap-api-endpoint'), '/custom/cap/v2/');
  assert.equal(ctx.window.CAP_CUSTOM_WASM_URL, '/custom/cap/v2/assets/cap_wasm_bg.wasm');
  assert.equal(ctx.window.CAP_CUSTOM_HASHWX_URL, '/custom/cap/v2/assets/hashwx.wasm');
  assert.equal(ctx.window.CAP_PAKO_URL, '/custom/cap/v2/assets/pako_inflate.min.js');
  for (const url of ['https://evil.example/custom/cap/v2/challenge', '/custom/cap/v2/validate', '/custom/cap/v2/challenge?x=1']) {
    await assert.rejects(ctx.window.CAP_CUSTOM_FETCH(url), /Unexpected verification endpoint/);
  }
}

// Language manager mirrors the original page (cookie + browser detect + attribute sync).
{
  const ctx = await setup();
  assert.equal(ctx.els['page-title'].textContent, 'Verification', 'browser language en by default');
  assert.equal(ctx.els['lang-switch'].textContent, '中文');
  assert.equal(ctx.els['info-title'].textContent, 'Verify');
  assert.equal(ctx.els['info-text'].textContent, 'Configured EN');
  assert.equal(ctx.widget.getAttribute('data-cap-i18n-initial-state'), "I'm a human");
  ctx.els['lang-switch'].emit('click');
  assert.match(ctx.document.cookie, /samwaf_lang=zh/, 'click toggles en->zh and persists');
  assert.equal(ctx.location.navigation, ctx.location.href, 'language switch reloads via fresh GET');
}

// Validate: single POST, strict success, GET navigation only after the 2s timer.
for (const method of ['GET', 'POST', 'PUT', 'HEAD', '']) {
  const ctx = await setup(method);
  ctx.widget.emit('solve', { token: 'one-use-token' });
  ctx.widget.emit('solve', { token: 'duplicate-token' });
  await tick(); await tick();
  assert.equal(ctx.calls.length, 1, `${method}: no duplicate validate`);
  assert.equal(ctx.calls[0].url, '/custom/cap/v2/validate');
  assert.equal(ctx.calls[0].options.method, 'POST');
  assert.equal(ctx.calls[0].options.body, '{"token":"one-use-token"}');
  assert.equal(ctx.calls[0].options.credentials, 'same-origin');
  assert.equal(ctx.calls[0].options.redirect, 'error');
  assert.equal(ctx.widget.style.display, 'none', 'widget hidden on success');
  assert.equal(ctx.els['lang-switch'].style.display, 'none');
  assert.ok(ctx.created.some(e => e.attributes.class === 'success-message' && e.textContent.includes('refresh')), 'success message shown');
  assert.equal(ctx.location.navigation, undefined, 'no navigation before the 2s timer');
  // The original page reloads after 2s; toast hiding also uses 2s timers. Fire
  // them all: the navigation timer must exist for GET and never for other methods.
  const navs = [...ctx.timers.values()].filter(t => t.ms === 2000);
  assert.ok(navs.length, 'original 2s reload timing kept');
  for (const t of navs) t.fn();
  if (method === 'GET') assert.equal(ctx.location.navigation, ctx.location.href);
  else assert.equal(ctx.location.navigation, undefined, `${method}: no automatic replay navigation`);
}

// Failure paths: no navigation, original toast semantics.
{
  const ctx = await setup('GET', { success: false });
  ctx.widget.emit('solve', { token: 'bad' });
  await tick(); await tick();
  assert.equal(ctx.location.navigation, undefined);
  assert.equal(ctx.widget.style.display, undefined, 'widget stays available for retry');
  const toast = ctx.created.find(e => e.classList._c?.has?.('toast'));
  assert.ok(toast && toast.textContent.includes('failed'), 'verifyFail toast');
}
{
  const ctx = await setup('GET', { success: true }, false);
  ctx.widget.emit('solve', { token: 'x' });
  await tick(); await tick();
  assert.equal(ctx.location.navigation, undefined);
  const toast = ctx.created.find(e => e.classList._c?.has?.('toast'));
  assert.ok(toast && toast.textContent.includes('error'), 'validationError toast');
}
{
  const ctx = await setup('GET');
  ctx.widget.emit('error');
  await tick();
  const toast = ctx.created.find(e => e.classList._c?.has?.('toast'));
  assert.ok(toast && toast.textContent.includes('failed'), 'widget error toast');
}
console.log('bridge tests passed');
