(function () {
  'use strict';

  // ---- modal (image detail) ----
  // Strings come from the server as a JSON block (see base.html), in the page
  // language; the English text is only a fallback. "{name}" marks a value.
  var strings = {};
  try { strings = JSON.parse(document.getElementById('i18n').textContent); } catch (e) { /* fallbacks */ }
  function tr(key, fallback, vars) {
    var s = strings[key] || fallback;
    if (vars) { Object.keys(vars).forEach(function (k) { s = s.split('{' + k + '}').join(vars[k]); }); }
    return s;
  }

  var modal = document.getElementById('modal');
  function closeModal() { if (modal) { modal.hidden = true; modal.innerHTML = ''; } }
  document.addEventListener('htmx:afterSwap', function (e) {
    if (e.target === modal) { modal.hidden = false; }
  });
  document.addEventListener('click', function (e) {
    var t = e.target;
    if (t === modal || (t.closest && t.closest('[data-close-modal]'))) { closeModal(); }
    var copy = t.closest && t.closest('[data-copy]');
    if (copy) {
      var input = document.querySelector(copy.getAttribute('data-copy'));
      if (input) {
        input.select();
        if (navigator.clipboard) { navigator.clipboard.writeText(input.value); } else { document.execCommand('copy'); }
        var label = copy.textContent;
        copy.textContent = tr('js.copied', 'Copied');
        setTimeout(function () { copy.textContent = label; }, 1500);
      }
    }
    if (t.closest && t.closest('[data-select-all]')) {
      var boxes = document.querySelectorAll('input.sel');
      var all = Array.prototype.every.call(boxes, function (b) { return b.checked; });
      boxes.forEach(function (b) { b.checked = !all; });
    }
  });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { closeModal(); } });

  // ---- image detail: previous/next and rating hotkeys ----
  // In the gallery modal, previous/next follow the tiles as shown in the folder
  // view (same filter and order); more pages are loaded when the end is reached.
  // The standalone image page keeps the server-rendered links (upload order).
  function inModal() { return modal && !modal.hidden && modal.querySelector('.detail'); }
  function tileNeighbour(dir) {
    var d = modal.querySelector('.detail');
    var n = d && document.getElementById('tile-' + d.getAttribute('data-id'));
    var sib = dir === 'prev' ? 'previousElementSibling' : 'nextElementSibling';
    n = n && n[sib];
    while (n && !n.classList.contains('tile')) { n = n[sib]; }
    return n ? n.id.replace('tile-', '') : null;
  }
  function updateNav() {
    if (!inModal()) { return; }
    var more = !!document.querySelector('.sentinel');
    modal.querySelectorAll('.navarrow').forEach(function (a) { a.remove(); });
    var d = modal.querySelector('.detail');
    [['prev', '‹', tileNeighbour('prev')], ['next', '›', tileNeighbour('next') || (more ? 'more' : null)]].forEach(function (x) {
      if (!x[2]) { return; }
      var a = document.createElement('a');
      a.className = 'navarrow ' + x[0];
      a.setAttribute('data-nav', x[0]);
      a.href = '#';
      a.textContent = x[1];
      a.title = x[0] === 'prev' ? tr('detail.prev_hint', 'Previous photo (←)') : tr('detail.next_hint', 'Next photo (→)');
      d.parentNode.insertBefore(a, d);
    });
  }
  var waitingForMore = false;
  function navigate(dir) {
    if (!inModal()) {
      var link = document.querySelector('a[data-nav="' + dir + '"]');
      if (link) { link.click(); }
      return;
    }
    var id = tileNeighbour(dir);
    if (id) {
      htmx.ajax('GET', '/images/' + id, { target: '#modal', swap: 'innerHTML' });
    } else if (dir === 'next') {
      var sentinel = document.querySelector('.sentinel');
      if (sentinel && !waitingForMore) { waitingForMore = true; sentinel.scrollIntoView(); }
    }
  }
  document.addEventListener('htmx:afterSettle', function () {
    if (waitingForMore && inModal() && tileNeighbour('next')) {
      waitingForMore = false;
      navigate('next');
      return;
    }
    waitingForMore = false;
    updateNav();
  });
  document.addEventListener('click', function (e) {
    var a = e.target.closest && e.target.closest('a[data-nav]');
    if (a && inModal()) { e.preventDefault(); navigate(a.getAttribute('data-nav')); }
  });
  document.addEventListener('keydown', function (e) {
    if (e.ctrlKey || e.metaKey || e.altKey || e.defaultPrevented) { return; }
    var t = e.target;
    if (t && t.closest && t.closest('input, textarea, select, [contenteditable]')) { return; }
    if (!document.querySelector('.detail')) { return; }
    if (e.key === 'ArrowLeft') { e.preventDefault(); navigate('prev'); }
    else if (e.key === 'ArrowRight') { e.preventDefault(); navigate('next'); }
    else if (/^[0-5]$/.test(e.key)) {
      var r = document.getElementById('rating');
      var btn = r && (e.key === '0' ? r.querySelector('.link') : r.querySelectorAll('.star')[+e.key - 1]);
      if (btn) { e.preventDefault(); btn.click(); }
    }
  });

  // Actions on the checked tiles: hx-confirm may contain "{n}", which is replaced
  // by the number of selected photos. Nothing selected means nothing to confirm.
  document.addEventListener('htmx:confirm', function (e) {
    var q = e.detail.question;
    if (!q || q.indexOf('{n}') < 0) { return; }
    e.preventDefault();
    var n = document.querySelectorAll('input.sel:checked').length;
    if (n === 0) { return; }
    if (window.confirm(q.replace('{n}', n))) { e.detail.issueRequest(true); }
  });

  // ---- anonymous upload ----
  // Each file goes to a resumable upload on the server in chunks (size chosen by
  // the server). A dropped or stalled request only costs the chunk in flight:
  // the script waits, continues where the server stopped and gives up only after
  // NETWORK_RETRIES failures in a row without progress.
  var zone = document.getElementById('dropzone');
  if (!zone) { return; }
  var input = document.getElementById('file-input');
  var list = document.getElementById('upload-list');
  var url = zone.getAttribute('data-url');
  var queue = [];
  var active = 0;
  var MAX_PARALLEL = 2;
  // The server answers 429 (rate limit) or 503 (busy) with Retry-After. Those are
  // not failures: pause all sending, then try the file again with back-off.
  var MAX_RETRIES = 10;
  var pausedUntil = 0;
  var pumpTimer = null;
  var NETWORK_RETRIES = 20;
  var STALL_MS = 30000; // a chunk without upload progress for this long is retried
  var REQUEST_MS = 120000; // start and complete requests
  var MAX_RESTARTS = 3;
  var waitingForNetwork = [];

  ['dragenter', 'dragover'].forEach(function (ev) {
    zone.addEventListener(ev, function (e) { e.preventDefault(); zone.classList.add('over'); });
  });
  ['dragleave', 'drop'].forEach(function (ev) {
    zone.addEventListener(ev, function (e) { e.preventDefault(); zone.classList.remove('over'); });
  });
  zone.addEventListener('drop', function (e) { add(e.dataTransfer.files); });
  input.addEventListener('change', function () { add(input.files); input.value = ''; });
  // Back online: retry right away instead of waiting out the back-off.
  if (typeof window !== 'undefined' && window.addEventListener) {
    window.addEventListener('online', function () {
      waitingForNetwork.splice(0).forEach(function (item) {
        clearTimeout(item.retryTimer);
        item.retryTimer = null;
        step(item);
      });
    });
  }

  function add(files) {
    Array.prototype.forEach.call(files, function (file) {
      var li = document.createElement('li');
      var label = document.createElement('div');
      label.textContent = file.name;
      var bar = document.createElement('progress');
      bar.max = 100; bar.value = 0;
      var status = document.createElement('div');
      status.className = 'small muted';
      status.textContent = tr('js.waiting', 'Waiting…');
      li.appendChild(label); li.appendChild(bar); li.appendChild(status);
      list.appendChild(li);
      queue.push({ file: file, bar: bar, status: status, id: null, offset: 0, failures: 0, restarts: 0 });
    });
    pump();
  }

  function pump() {
    var wait = pausedUntil - Date.now();
    if (wait > 0) {
      if (!pumpTimer) { pumpTimer = setTimeout(function () { pumpTimer = null; pump(); }, wait); }
      return;
    }
    while (active < MAX_PARALLEL && queue.length) { send(queue.shift()); }
  }

  function finish(item, ok, msg) {
    active--;
    item.status.textContent = msg;
    item.status.className = 'small ' + (ok ? 'ok' : 'error');
    if (ok) { item.bar.value = 100; }
    pump();
  }

  function showStatus(item, msg) {
    item.status.className = 'small muted';
    item.status.textContent = msg;
  }

  function progress(item, inFlight) {
    var size = item.file.size || 1;
    item.bar.value = ((item.offset + (inFlight || 0)) / size) * 100;
  }

  // Seconds to wait: the server's Retry-After if it sent one, else 2, 4, 8, ... (max 60),
  // plus up to 2 s of jitter so parallel clients behind one NAT do not retry in lockstep.
  function retryDelay(header, attempt) {
    var secs = parseInt(header, 10);
    if (!(secs > 0)) { secs = Math.pow(2, attempt); }
    return Math.min(secs, 60) + Math.random() * 2;
  }

  function retryLater(item, xhr) {
    item.attempts = (item.attempts || 0) + 1;
    if (item.attempts > MAX_RETRIES) {
      finish(item, false, tr('js.busy_give_up', 'The server is busy, please try again later'));
      return;
    }
    var delay = retryDelay(xhr.getResponseHeader('Retry-After'), item.attempts);
    active--;
    pausedUntil = Math.max(pausedUntil, Date.now() + delay * 1000);
    progress(item, 0);
    showStatus(item, tr('js.busy_retry', 'Server busy, retrying in {n} s…', { n: Math.ceil(delay) }));
    queue.unshift(item);
    pump();
  }

  // The connection failed: wait 2, 4, 8, ... (max 30) s and continue. The file
  // keeps its slot so the other files do not run into the same outage.
  function networkRetry(item) {
    item.failures++;
    if (item.failures > NETWORK_RETRIES) {
      finish(item, false, tr('js.network_error', 'Network error'));
      return;
    }
    var delay = Math.min(Math.pow(2, item.failures), 30) + Math.random() * 2;
    progress(item, 0);
    showStatus(item, tr('js.network_retry', 'Connection lost, retrying in {n} s…', { n: Math.ceil(delay) }));
    waitingForNetwork.push(item);
    item.retryTimer = setTimeout(function () {
      item.retryTimer = null;
      var i = waitingForNetwork.indexOf(item);
      if (i >= 0) { waitingForNetwork.splice(i, 1); }
      step(item);
    }, delay * 1000);
  }

  // The server no longer knows the upload (restarted, or it was abandoned too
  // long): send the file again from the start.
  function restart(item) {
    item.restarts++;
    if (item.restarts > MAX_RESTARTS) {
      finish(item, false, tr('js.failed', 'Upload failed ({status})', { status: 404 }));
      return;
    }
    item.id = null;
    item.offset = 0;
    step(item);
  }

  function fail(item, xhr, res) {
    var r = res && res.results && res.results[0];
    if (r && r.error) { finish(item, false, r.error); }
    else if (res && res.error) { finish(item, false, res.error); }
    else if (xhr.status === 410) { finish(item, false, tr('js.expired', 'This upload link has expired')); }
    else if (xhr.status === 413) { finish(item, false, tr('js.too_large', 'File is too large')); }
    else { finish(item, false, tr('js.failed', 'Upload failed ({status})', { status: xhr.status })); }
  }

  // request sends one XHR. Rate limits and busy answers go to retryLater, lost
  // connections (including a gateway that could not reach the server) to
  // networkRetry; everything else to onload with the parsed JSON body, if any.
  function request(item, opts, onload) {
    var xhr = new XMLHttpRequest();
    var settled = false;
    var watchdog = null;
    function done() {
      if (settled) { return false; }
      settled = true;
      if (watchdog) { clearTimeout(watchdog); }
      return true;
    }
    function lost() { if (done()) { networkRetry(item); } }
    function arm() {
      if (watchdog) { clearTimeout(watchdog); }
      watchdog = setTimeout(function () { if (!settled) { xhr.abort(); lost(); } }, opts.stallMs);
    }
    xhr.open(opts.method, opts.url);
    Object.keys(opts.headers || {}).forEach(function (k) { xhr.setRequestHeader(k, opts.headers[k]); });
    if (opts.timeout) { xhr.timeout = opts.timeout; }
    xhr.onerror = lost;
    xhr.ontimeout = lost;
    xhr.onabort = lost;
    if (opts.stallMs) {
      xhr.upload.onprogress = function (e) {
        arm();
        if (opts.onprogress) { opts.onprogress(e); }
      };
    }
    xhr.onload = function () {
      if (!done()) { return; }
      if (xhr.status === 429 || xhr.status === 503) { retryLater(item, xhr); return; }
      if (xhr.status === 0 || xhr.status === 502 || xhr.status === 504) { networkRetry(item); return; }
      var res = null;
      try { res = JSON.parse(xhr.responseText); } catch (e) { /* not JSON */ }
      onload(xhr, res);
    };
    xhr.send(opts.body === undefined ? null : opts.body);
    if (opts.stallMs) { arm(); }
  }

  function send(item) {
    active++;
    showStatus(item, tr('js.uploading', 'Uploading…'));
    step(item);
  }

  function step(item) {
    if (!item.id) { start(item); }
    else if (item.offset < item.file.size) { putChunk(item); }
    else { complete(item); }
  }

  // Files up to this size are hashed first, so the server can tell when it
  // already has an exact copy and nothing has to be sent. The digest needs the
  // whole file in memory, larger files are just uploaded.
  var HASH_MAX = 25 * 1024 * 1024;

  // hash calls done with the file's SHA-256 as lowercase hex, or with "" when
  // it cannot be computed (no crypto.subtle outside HTTPS, file too large, read
  // error); the upload then goes ahead without the check.
  function hash(item, done) {
    var subtle = window.crypto && window.crypto.subtle;
    if (!subtle || !item.file.arrayBuffer || item.file.size > HASH_MAX) { done(''); return; }
    item.file.arrayBuffer().then(function (buf) {
      return subtle.digest('SHA-256', buf);
    }).then(function (sum) {
      done(Array.prototype.map.call(new Uint8Array(sum), function (b) {
        return ('0' + b.toString(16)).slice(-2);
      }).join(''));
    }, function () { done(''); });
  }

  function start(item) {
    if (item.sha256 === undefined) {
      hash(item, function (sum) { item.sha256 = sum; start(item); });
      return;
    }
    var body = { name: item.file.name, size: item.file.size };
    if (item.sha256) { body.sha256 = item.sha256; }
    request(item, {
      method: 'POST', url: url, timeout: REQUEST_MS,
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify(body),
    }, function (xhr, res) {
      var r = res && res.results && res.results[0];
      if (xhr.status === 200 && r && r.ok && r.duplicate) {
        finish(item, true, tr('js.already_uploaded', 'Already uploaded, skipped'));
      } else if (xhr.status === 201 && res && res.id) {
        item.id = res.id;
        item.chunk = res.chunk_size;
        item.offset = res.offset || 0;
        item.failures = 0;
        step(item);
      } else { fail(item, xhr, res); }
    });
  }

  function putChunk(item) {
    var end = Math.min(item.offset + item.chunk, item.file.size);
    showStatus(item, tr('js.uploading', 'Uploading…'));
    request(item, {
      method: 'PUT', url: url + '/' + item.id, stallMs: STALL_MS,
      headers: { 'Upload-Offset': String(item.offset) },
      body: item.file.slice(item.offset, end),
      onprogress: function (e) { progress(item, e.loaded); },
    }, function (xhr, res) {
      var known = res && typeof res.offset === 'number';
      if ((xhr.status === 200 || xhr.status === 409) && known) {
        // 409: part of an earlier attempt arrived after all; continue after it.
        if (res.offset > item.offset) { item.failures = 0; }
        item.offset = res.offset;
        progress(item, 0);
        step(item);
      } else if (xhr.status === 404) { restart(item); }
      else if (xhr.status === 400 && known) {
        item.offset = res.offset;
        networkRetry(item);
      } else { fail(item, xhr, res); }
    });
  }

  function complete(item) {
    showStatus(item, tr('js.processing', 'Processing…'));
    request(item, { method: 'POST', url: url + '/' + item.id + '/complete', timeout: REQUEST_MS }, function (xhr, res) {
      var r = res && res.results && res.results[0];
      if (xhr.status === 200 && r && r.ok) { finish(item, true, tr('js.uploaded', 'Uploaded')); }
      else if (xhr.status === 409 && res && typeof res.offset === 'number') {
        item.offset = res.offset;
        step(item);
      } else if (xhr.status === 404 && !r) { restart(item); }
      else { fail(item, xhr, res); }
    });
  }
})();
