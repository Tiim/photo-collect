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

  ['dragenter', 'dragover'].forEach(function (ev) {
    zone.addEventListener(ev, function (e) { e.preventDefault(); zone.classList.add('over'); });
  });
  ['dragleave', 'drop'].forEach(function (ev) {
    zone.addEventListener(ev, function (e) { e.preventDefault(); zone.classList.remove('over'); });
  });
  zone.addEventListener('drop', function (e) { add(e.dataTransfer.files); });
  input.addEventListener('change', function () { add(input.files); input.value = ''; });

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
      queue.push({ file: file, bar: bar, status: status });
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
    item.bar.value = 0;
    item.status.className = 'small muted';
    item.status.textContent = tr('js.busy_retry', 'Server busy, retrying in {n} s…', { n: Math.ceil(delay) });
    queue.unshift(item);
    pump();
  }

  function send(item) {
    active++;
    item.status.textContent = tr('js.uploading', 'Uploading…');
    var xhr = new XMLHttpRequest();
    xhr.open('POST', url);
    xhr.upload.onprogress = function (e) {
      if (e.lengthComputable) { item.bar.value = (e.loaded / e.total) * 100; }
    };
    xhr.onerror = function () { finish(item, false, tr('js.network_error', 'Network error')); };
    xhr.onload = function () {
      if (xhr.status === 429 || xhr.status === 503) { retryLater(item, xhr); return; }
      var res = null;
      try { res = JSON.parse(xhr.responseText).results[0]; } catch (e) { /* not JSON */ }
      if (res && res.ok) { finish(item, true, tr('js.uploaded', 'Uploaded')); }
      else if (res && res.error) { finish(item, false, res.error); }
      else if (xhr.status === 410) { finish(item, false, tr('js.expired', 'This upload link has expired')); }
      else if (xhr.status === 413) { finish(item, false, tr('js.too_large', 'File is too large')); }
      else { finish(item, false, tr('js.failed', 'Upload failed ({status})', { status: xhr.status })); }
    };
    var fd = new FormData();
    fd.append('files', item.file, item.file.name);
    xhr.send(fd);
  }
})();
