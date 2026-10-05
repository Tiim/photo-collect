// Runs web/static/app.js against a minimal fake DOM/XHR and checks how the
// chunked upload reacts to 429/503, dropped connections and stalls. Exit code 1
// on failure.
'use strict';
const fs = require('fs');
const vm = require('vm');
const assert = require('assert');

const src = fs.readFileSync(process.argv[2], 'utf8');

function el() {
  const e = { children: [], attrs: {}, listeners: {}, classList: { add() {}, remove() {} }, textContent: '', className: '', value: 0 };
  e.appendChild = (c) => { e.children.push(c); return c; };
  e.addEventListener = (n, f) => { (e.listeners[n] = e.listeners[n] || []).push(f); };
  e.getAttribute = (k) => e.attrs[k];
  return e;
}

function file(name, size) {
  return { name, size, slice: (a, b) => ({ slice: [a, b] }), arrayBuffer: () => Promise.resolve(new ArrayBuffer(size)) };
}

function scenario(name, fn, strings) {
  let clock = 1000000;
  let timers = [];
  let timerID = 0;
  const xhrs = [];
  const els = { dropzone: el(), 'file-input': el(), 'upload-list': el() };
  if (strings) els.i18n = Object.assign(el(), { textContent: JSON.stringify(strings) });
  els.dropzone.attrs['data-url'] = '/upload/T/chunked';
  const win = el();
  const doc = { getElementById: (id) => els[id] || null, createElement: () => el(), addEventListener() {}, querySelectorAll: () => [] };
  function XHR() {
    this.upload = {}; this.headers = {}; this.reqHeaders = {};
    xhrs.push(this);
  }
  XHR.prototype.open = function (m, u) { this.method = m; this.url = u; };
  XHR.prototype.setRequestHeader = function (k, v) { this.reqHeaders[k] = v; };
  XHR.prototype.send = function (body) { this.sent = true; this.body = body; };
  XHR.prototype.abort = function () { this.aborted = true; if (this.onabort) this.onabort(); };
  XHR.prototype.getResponseHeader = function (k) { return this.headers[k] === undefined ? null : this.headers[k]; };
  function reply(x, status, body, headers) {
    x.status = status;
    x.responseText = typeof body === 'string' ? body : JSON.stringify(body);
    x.headers = headers || {};
    x.onload();
  }
  const ctx = {
    document: doc, window: win, XMLHttpRequest: XHR, JSON,
    Math: Object.assign(Object.create(Math), { random: () => 0.5 }),
    Date: { now: () => clock },
    setTimeout: (f, ms) => { timers.push({ id: ++timerID, at: clock + ms, f }); return timerID; },
    clearTimeout: (id) => { timers = timers.filter((t) => t.id !== id); },
  };
  vm.createContext(ctx);
  vm.runInContext(src, ctx);
  const h = {
    xhrs, els, reply,
    last: () => xhrs[xhrs.length - 1],
    pick(n, size) {
      const files = [];
      for (let i = 0; i < n; i++) files.push(file('f' + i + '.jpg', size || 10));
      els['file-input'].files = files;
      els['file-input'].listeners.change[0]();
    },
    // upload drives a just-started file through start, one chunk and complete.
    upload(startXHR, size) {
      reply(startXHR, 201, { id: 'u', chunk_size: 100, offset: 0 });
      reply(h.last(), 200, { offset: size || 10 });
      reply(h.last(), 200, ok);
    },
    status: (i) => els['upload-list'].children[i].children[2].textContent,
    bar: (i) => els['upload-list'].children[i].children[1].value,
    online: () => win.listeners.online.forEach((f) => f()),
    advance(ms) {
      clock += ms;
      for (;;) {
        const due = timers.filter((t) => t.at <= clock);
        if (!due.length) return;
        timers = timers.filter((t) => t.at > clock);
        due.forEach((t) => t.f());
      }
    },
  };
  const fail = (e) => { console.log('FAIL ' + name + ': ' + (e.stack || e)); process.exitCode = 1; };
  let res;
  try { res = fn(h, win); } catch (e) { fail(e); return; }
  if (res && res.then) { pending = pending.then(() => res).then(() => console.log('ok   ' + name), fail); } else { console.log('ok   ' + name); }
}

let pending = Promise.resolve();
// settle lets the promises of the hashing step run.
const settle = () => new Promise((r) => setImmediate(r));

// fakeCrypto stands in for crypto.subtle; digest returns 32 bytes of seed.
function fakeCrypto(win, seed, fail) {
  win.crypto = { subtle: { digest: (alg, buf) => {
    assert.strictEqual(alg, 'SHA-256');
    assert.ok(buf.byteLength >= 0);
    return fail ? Promise.reject(new Error('no')) : Promise.resolve(new Uint8Array(32).fill(seed).buffer);
  } } };
}

const ok = { results: [{ name: 'f.jpg', ok: true }] };

scenario('a file is sent in chunks and then completed', (h) => {
  h.pick(1, 10);
  const start = h.xhrs[0];
  assert.strictEqual(start.method, 'POST');
  assert.strictEqual(start.url, '/upload/T/chunked');
  assert.deepStrictEqual(JSON.parse(start.body), { name: 'f0.jpg', size: 10 });
  h.reply(start, 201, { id: 'abc', chunk_size: 4, offset: 0 });
  for (const [from, to] of [[0, 4], [4, 8], [8, 10]]) {
    const put = h.last();
    assert.strictEqual(put.method, 'PUT');
    assert.strictEqual(put.url, '/upload/T/chunked/abc');
    assert.strictEqual(put.reqHeaders['Upload-Offset'], String(from));
    assert.deepStrictEqual(put.body.slice, [from, to]);
    put.upload.onprogress({ loaded: (to - from) / 2 });
    assert.strictEqual(h.bar(0), ((from + (to - from) / 2) / 10) * 100);
    h.reply(put, 200, { offset: to });
  }
  const done = h.last();
  assert.strictEqual(done.url, '/upload/T/chunked/abc/complete');
  assert.strictEqual(h.status(0), 'Processing…');
  h.reply(done, 200, ok);
  assert.strictEqual(h.status(0), 'Uploaded');
  assert.strictEqual(h.xhrs.length, 5);
});

scenario('a dropped chunk is resumed where the server stopped', (h) => {
  h.pick(1, 10);
  h.reply(h.xhrs[0], 201, { id: 'abc', chunk_size: 6, offset: 0 });
  h.last().onerror();
  assert.match(h.status(0), /Connection lost, retrying in 3 s/);
  h.advance(2900);
  assert.strictEqual(h.xhrs.length, 2, 'waits before retrying');
  h.advance(200);
  assert.strictEqual(h.xhrs.length, 3);
  // Part of the dropped request had arrived: continue after it.
  h.reply(h.last(), 409, { offset: 4 });
  assert.strictEqual(h.last().reqHeaders['Upload-Offset'], '4');
  assert.deepStrictEqual(h.last().body.slice, [4, 10]);
  h.reply(h.last(), 200, { offset: 10 });
  h.reply(h.last(), 200, ok);
  assert.strictEqual(h.status(0), 'Uploaded');
});

scenario('gateway errors and timeouts are network failures', (h) => {
  h.pick(1, 10);
  h.reply(h.xhrs[0], 201, { id: 'abc', chunk_size: 100, offset: 0 });
  h.reply(h.last(), 502, '<html>Bad Gateway</html>');
  assert.match(h.status(0), /Connection lost/);
  h.advance(3100);
  h.reply(h.last(), 200, { offset: 10 });
  h.last().ontimeout(); // complete timed out
  h.advance(10000);
  assert.strictEqual(h.last().url, '/upload/T/chunked/abc/complete', 'complete is asked again');
  h.reply(h.last(), 200, ok);
  assert.strictEqual(h.status(0), 'Uploaded');
});

scenario('a stalled chunk is aborted and retried', (h) => {
  h.pick(1, 10);
  h.reply(h.xhrs[0], 201, { id: 'abc', chunk_size: 100, offset: 0 });
  const put = h.last();
  h.advance(20000);
  put.upload.onprogress({ loaded: 3 }); // progress resets the watchdog
  h.advance(20000);
  assert.ok(!put.aborted, 'still making progress');
  h.advance(10100);
  assert.ok(put.aborted, 'no progress for 30 s');
  assert.match(h.status(0), /Connection lost/);
  assert.strictEqual(h.xhrs.length, 2);
  h.advance(3100);
  assert.strictEqual(h.xhrs.length, 3);
  assert.strictEqual(h.last().method, 'PUT');
});

scenario('coming back online retries immediately', (h) => {
  h.pick(1, 10);
  h.reply(h.xhrs[0], 201, { id: 'abc', chunk_size: 100, offset: 0 });
  for (let i = 0; i < 4; i++) { h.last().onerror(); h.advance(40000); }
  h.last().onerror();
  const n = h.xhrs.length;
  h.online();
  assert.strictEqual(h.xhrs.length, n + 1);
  h.advance(60000);
  assert.strictEqual(h.xhrs.length, n + 1, 'the cancelled back-off does not fire again');
});

scenario('an upload the server forgot starts over', (h) => {
  h.pick(1, 10);
  h.reply(h.xhrs[0], 201, { id: 'abc', chunk_size: 4, offset: 0 });
  h.reply(h.last(), 200, { offset: 4 });
  h.reply(h.last(), 404, { offset: 0, error: 'The upload was interrupted' });
  assert.strictEqual(h.last().url, '/upload/T/chunked');
  h.reply(h.last(), 201, { id: 'def', chunk_size: 100, offset: 0 });
  assert.strictEqual(h.last().url, '/upload/T/chunked/def');
  assert.strictEqual(h.last().reqHeaders['Upload-Offset'], '0');
});

scenario('gives up after many network failures without progress', (h) => {
  h.pick(1, 10);
  h.reply(h.xhrs[0], 201, { id: 'abc', chunk_size: 100, offset: 0 });
  for (let i = 0; i < 20; i++) {
    h.last().onerror();
    assert.match(h.status(0), /Connection lost/);
    h.advance(40000);
  }
  h.last().onerror();
  assert.strictEqual(h.status(0), 'Network error');
  h.advance(120000);
  assert.strictEqual(h.xhrs.length, 22);
});

scenario('progress resets the failure count', (h) => {
  h.pick(1, 100);
  h.reply(h.xhrs[0], 201, { id: 'abc', chunk_size: 10, offset: 0 });
  for (let i = 0; i < 40; i++) {
    h.last().onerror();
    h.advance(40000);
    h.reply(h.last(), 409, { offset: Math.min(100, (i + 1) * 5) });
    if (h.last().url.endsWith('/complete')) break;
  }
  assert.strictEqual(h.status(0), 'Processing…');
});

scenario('429 with Retry-After is retried, not failed', (h) => {
  h.pick(1);
  assert.strictEqual(h.xhrs.length, 1);
  h.reply(h.xhrs[0], 429, 'Too many requests', { 'Retry-After': '3' });
  assert.match(h.status(0), /retrying/);
  assert.doesNotMatch(h.status(0), /failed/i);
  h.advance(2900);
  assert.strictEqual(h.xhrs.length, 1, 'must wait for Retry-After');
  h.advance(2100); // 3 s + jitter (1 s with the stubbed random)
  assert.strictEqual(h.xhrs.length, 2, 'file is sent again');
  h.upload(h.xhrs[1]);
  assert.strictEqual(h.status(0), 'Uploaded');
});

scenario('a busy complete keeps the uploaded bytes', (h) => {
  h.pick(1, 10);
  h.reply(h.xhrs[0], 201, { id: 'abc', chunk_size: 100, offset: 0 });
  h.reply(h.last(), 200, { offset: 10 });
  h.reply(h.last(), 503, { results: [{ error: 'busy' }] }, { 'Retry-After': '5' });
  h.advance(6100);
  assert.strictEqual(h.xhrs.length, 4);
  assert.strictEqual(h.last().url, '/upload/T/chunked/abc/complete');
});

scenario('503 without header backs off exponentially', (h) => {
  h.pick(1);
  h.reply(h.xhrs[0], 503, { results: [{ error: 'busy' }] });
  h.advance(2500); // first back-off is 2 s + 1 s (stubbed jitter)
  assert.strictEqual(h.xhrs.length, 1);
  h.advance(600);
  assert.strictEqual(h.xhrs.length, 2);
  h.reply(h.xhrs[1], 503, '');
  h.advance(3900);
  assert.strictEqual(h.xhrs.length, 2, 'second back-off is longer (4 s + jitter)');
  h.advance(1200);
  assert.strictEqual(h.xhrs.length, 3);
});

scenario('a rate limit pauses queued files too', (h) => {
  h.pick(3); // two in flight, one waiting
  assert.strictEqual(h.xhrs.length, 2);
  const [a, b] = h.xhrs;
  h.reply(a, 429, '', { 'Retry-After': '5' });
  h.upload(b);
  assert.strictEqual(h.status(1), 'Uploaded');
  assert.strictEqual(h.xhrs.length, 4, 'nothing new is started while paused');
  h.advance(6100);
  assert.strictEqual(h.xhrs.length, 6, 'retry and the waiting file go out after the pause');
});

scenario('gives up after too many busy retries', (h) => {
  h.pick(1);
  for (let i = 0; i < 11; i++) {
    h.reply(h.xhrs[i], 429, '', { 'Retry-After': '1' });
    h.advance(3000);
  }
  assert.match(h.status(0), /busy, please try again later/);
  assert.strictEqual(h.xhrs.length, 11);
});

scenario('real errors are not retried', (h) => {
  h.pick(1);
  h.reply(h.xhrs[0], 400, { results: [{ ok: false, error: 'File is too large (max 50 MB)' }] });
  h.advance(120000);
  assert.strictEqual(h.xhrs.length, 1);
  assert.strictEqual(h.status(0), 'File is too large (max 50 MB)');
  h.pick(1);
  h.reply(h.xhrs[1], 403, { results: [{ error: 'Please enter a nickname first' }] });
  assert.strictEqual(h.status(1), 'Please enter a nickname first');
  h.pick(1);
  h.reply(h.xhrs[2], 201, { id: 'abc', chunk_size: 100, offset: 0 });
  h.reply(h.last(), 200, { offset: 10 });
  h.reply(h.last(), 200, { results: [{ ok: false, error: 'File is not a supported image' }] });
  assert.strictEqual(h.status(2), 'File is not a supported image');
  h.pick(1);
  h.reply(h.last(), 410, '<html>expired</html>');
  assert.strictEqual(h.status(3), 'This upload link has expired');
});

scenario('texts come from the page language', (h) => {
  h.pick(1);
  assert.strictEqual(h.status(0), 'Wird hochgeladen…');
  h.reply(h.xhrs[0], 429, '', { 'Retry-After': '4' });
  assert.strictEqual(h.status(0), 'Server ausgelastet, neuer Versuch in 5 s…');
  h.advance(6000);
  h.reply(h.xhrs[1], 201, { id: 'abc', chunk_size: 100, offset: 0 });
  h.last().onerror();
  assert.strictEqual(h.status(0), 'Verbindung unterbrochen, neuer Versuch in 3 s…');
  h.advance(3100);
  h.reply(h.last(), 200, { offset: 10 });
  h.reply(h.last(), 200, ok);
  assert.strictEqual(h.status(0), 'Hochgeladen');
}, {
  'js.uploading': 'Wird hochgeladen…', 'js.busy_retry': 'Server ausgelastet, neuer Versuch in {n} s…', 'js.uploaded': 'Hochgeladen',
  'js.network_retry': 'Verbindung unterbrochen, neuer Versuch in {n} s…',
});

scenario('a file the server already has is not sent', async (h, win) => {
  fakeCrypto(win, 0xab);
  h.pick(1, 10);
  assert.strictEqual(h.xhrs.length, 0, 'hashes before starting');
  await settle();
  const start = h.xhrs[0];
  assert.deepStrictEqual(JSON.parse(start.body), { name: 'f0.jpg', size: 10, sha256: 'ab'.repeat(32) });
  h.reply(start, 200, { results: [{ name: 'f0.jpg', ok: true, duplicate: true }] });
  assert.strictEqual(h.status(0), 'Already uploaded, skipped');
  assert.strictEqual(h.bar(0), 100);
  assert.strictEqual(h.xhrs.length, 1, 'no chunks');
});

scenario('an unknown hash uploads as usual and is not computed twice', async (h, win) => {
  fakeCrypto(win, 0x01);
  let digests = 0;
  const digest = win.crypto.subtle.digest;
  win.crypto.subtle.digest = (...a) => { digests++; return digest(...a); };
  h.pick(1, 10);
  await settle();
  h.reply(h.xhrs[0], 429, '', { 'Retry-After': '1' });
  h.advance(3000);
  assert.strictEqual(JSON.parse(h.last().body).sha256, '01'.repeat(32), 'retry keeps the hash');
  h.upload(h.last());
  assert.strictEqual(h.status(0), 'Uploaded');
  assert.strictEqual(digests, 1);
});

scenario('without a hash the file is uploaded anyway', async (h, win) => {
  fakeCrypto(win, 0, true);
  h.pick(1, 10);
  await settle();
  assert.deepStrictEqual(JSON.parse(h.xhrs[0].body), { name: 'f0.jpg', size: 10 }, 'digest failed');
  fakeCrypto(win, 0x02);
  h.pick(1, 30 * 1024 * 1024);
  assert.deepStrictEqual(JSON.parse(h.last().body), { name: 'f0.jpg', size: 30 * 1024 * 1024 }, 'too large to hash');
});
