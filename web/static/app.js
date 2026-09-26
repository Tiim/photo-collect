(function () {
  'use strict';

  // ---- modal (image detail) ----
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
        copy.textContent = 'Copied';
        setTimeout(function () { copy.textContent = 'Copy'; }, 1500);
      }
    }
    if (t.closest && t.closest('[data-select-all]')) {
      var boxes = document.querySelectorAll('input.sel');
      var all = Array.prototype.every.call(boxes, function (b) { return b.checked; });
      boxes.forEach(function (b) { b.checked = !all; });
    }
  });
  document.addEventListener('keydown', function (e) { if (e.key === 'Escape') { closeModal(); } });

  // ---- anonymous upload ----
  var zone = document.getElementById('dropzone');
  if (!zone) { return; }
  var input = document.getElementById('file-input');
  var list = document.getElementById('upload-list');
  var url = zone.getAttribute('data-url');
  var queue = [];
  var active = 0;
  var MAX_PARALLEL = 2;

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
      status.textContent = 'Waiting…';
      li.appendChild(label); li.appendChild(bar); li.appendChild(status);
      list.appendChild(li);
      queue.push({ file: file, bar: bar, status: status });
    });
    pump();
  }

  function pump() {
    while (active < MAX_PARALLEL && queue.length) { send(queue.shift()); }
  }

  function finish(item, ok, msg) {
    active--;
    item.status.textContent = msg;
    item.status.className = 'small ' + (ok ? 'ok' : 'error');
    if (ok) { item.bar.value = 100; }
    pump();
  }

  function send(item) {
    active++;
    item.status.textContent = 'Uploading…';
    var xhr = new XMLHttpRequest();
    xhr.open('POST', url);
    xhr.upload.onprogress = function (e) {
      if (e.lengthComputable) { item.bar.value = (e.loaded / e.total) * 100; }
    };
    xhr.onerror = function () { finish(item, false, 'Network error'); };
    xhr.onload = function () {
      var res = null;
      try { res = JSON.parse(xhr.responseText).results[0]; } catch (e) { /* not JSON */ }
      if (res && res.ok) { finish(item, true, 'Uploaded'); }
      else if (res && res.error) { finish(item, false, res.error); }
      else if (xhr.status === 410) { finish(item, false, 'This upload link has expired'); }
      else if (xhr.status === 413) { finish(item, false, 'File is too large'); }
      else { finish(item, false, 'Upload failed (' + xhr.status + ')'); }
    };
    var fd = new FormData();
    fd.append('files', item.file, item.file.name);
    xhr.send(fd);
  }
})();
