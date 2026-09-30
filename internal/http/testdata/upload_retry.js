// Runs web/static/app.js against a minimal fake DOM/XHR and checks how the
// upload queue reacts to 429/503. Exit code 1 on failure.
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

function scenario(name, fn, strings) {
  let clock = 1000000;
  let timers = [];
  const xhrs = [];
  const els = { dropzone: el(), 'file-input': el(), 'upload-list': el() };
  if (strings) els.i18n = Object.assign(el(), { textContent: JSON.stringify(strings) });
  els.dropzone.attrs['data-url'] = '/upload/T/images';
  const doc = { getElementById: (id) => els[id] || null, createElement: () => el(), addEventListener() {}, querySelectorAll: () => [] };
  function XHR() {
    this.upload = {}; this.headers = {};
    xhrs.push(this);
  }
  XHR.prototype.open = function (m, u) { this.method = m; this.url = u; };
  XHR.prototype.send = function () { this.sent = true; };
  XHR.prototype.getResponseHeader = function (k) { return this.headers[k] === undefined ? null : this.headers[k]; };
  function reply(x, status, body, headers) {
    x.status = status; x.responseText = body || ''; x.headers = headers || {};
    x.onload();
  }
  const ctx = {
    document: doc, XMLHttpRequest: XHR, FormData: function () { this.append = () => {}; },
    Math: Object.assign(Object.create(Math), { random: () => 0.5 }),
    Date: { now: () => clock },
    setTimeout: (f, ms) => { timers.push({ at: clock + ms, f }); return timers.length; },
  };
  vm.createContext(ctx);
  vm.runInContext(src, ctx);
  const h = {
    xhrs, els, reply,
    pick(n) {
      const files = [];
      for (let i = 0; i < n; i++) files.push({ name: 'f' + i + '.jpg' });
      els['file-input'].files = files;
      els['file-input'].listeners.change[0]();
    },
    status: (i) => els['upload-list'].children[i].children[2].textContent,
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
  try { fn(h); console.log('ok   ' + name); } catch (e) { console.log('FAIL ' + name + ': ' + (e.stack || e)); process.exitCode = 1; }
}

const ok = JSON.stringify({ results: [{ name: 'f.jpg', ok: true }] });

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
  h.reply(h.xhrs[1], 200, ok);
  assert.strictEqual(h.status(0), 'Uploaded');
});

scenario('503 without header backs off exponentially', (h) => {
  h.pick(1);
  h.reply(h.xhrs[0], 503, JSON.stringify({ results: [{ error: 'busy' }] }));
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
  h.reply(h.xhrs[0], 429, '', { 'Retry-After': '5' });
  h.reply(h.xhrs[1], 200, ok);
  assert.strictEqual(h.xhrs.length, 2, 'nothing is sent while paused');
  h.advance(6100);
  assert.strictEqual(h.xhrs.length, 4, 'retry and the waiting file go out after the pause');
});

scenario('gives up after too many retries', (h) => {
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
  h.reply(h.xhrs[0], 200, JSON.stringify({ results: [{ ok: false, error: 'File is too large (max 50 MB)' }] }));
  h.advance(120000);
  assert.strictEqual(h.xhrs.length, 1);
  assert.strictEqual(h.status(0), 'File is too large (max 50 MB)');
  h.pick(1);
  h.reply(h.xhrs[1], 403, JSON.stringify({ results: [{ error: 'Please enter a nickname first' }] }));
  assert.strictEqual(h.status(1), 'Please enter a nickname first');
});

scenario('texts come from the page language', (h) => {
  h.pick(1);
  assert.strictEqual(h.status(0), 'Wird hochgeladen…');
  h.reply(h.xhrs[0], 429, '', { 'Retry-After': '4' });
  assert.strictEqual(h.status(0), 'Server ausgelastet, neuer Versuch in 5 s…');
  h.advance(6000);
  h.reply(h.xhrs[1], 200, ok);
  assert.strictEqual(h.status(0), 'Hochgeladen');
}, { 'js.uploading': 'Wird hochgeladen…', 'js.busy_retry': 'Server ausgelastet, neuer Versuch in {n} s…', 'js.uploaded': 'Hochgeladen' });
