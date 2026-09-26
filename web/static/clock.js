// Clock page: shows the current time as text and as a QR code so that it can be
// photographed with every camera. Uploaded photos of this page are used to
// correct the timestamps of all photos from the same device.
//
// QR payload (parsed by internal/images/qr.go):
//   PC1|<utc unix seconds>|<local wall time YYYY-MM-DDTHH:MM:SS>
(function () {
  'use strict';

  var timeEl = document.getElementById('clock-time');
  var dateEl = document.getElementById('clock-date');
  var qrEl = document.getElementById('clock-qr');
  var statusEl = document.getElementById('clock-status');
  if (!timeEl || !qrEl || typeof qrcode === 'undefined') { return; }

  // Milliseconds to add to this device's clock to get server time. The device
  // showing the page may itself have a wrong clock, so we never trust it.
  var skew = 0;
  var synced = false;

  function pad(n) { return (n < 10 ? '0' : '') + n; }
  function now() { return Date.now() + skew; }

  function sync() {
    var t0 = Date.now();
    fetch('/time', { cache: 'no-store' }).then(function (r) { return r.json(); }).then(function (j) {
      var t1 = Date.now();
      // Assume the server stamped the response halfway through the round trip.
      skew = j.ms - (t0 + t1) / 2;
      synced = true;
      statusEl.textContent = 'Synchronised with the server.';
    }).catch(function () {
      statusEl.textContent = synced ? '' : 'Could not reach the server. This device’s own clock is shown.';
    });
  }

  function render() {
    var sec = Math.floor(now() / 1000);
    var d = new Date(sec * 1000); // wall time fields use this browser's time zone
    var date = d.getFullYear() + '-' + pad(d.getMonth() + 1) + '-' + pad(d.getDate());
    var time = pad(d.getHours()) + ':' + pad(d.getMinutes()) + ':' + pad(d.getSeconds());
    dateEl.textContent = date;
    timeEl.textContent = time;

    var qr = qrcode(0, 'M');
    qr.addData('PC1|' + sec + '|' + date + 'T' + time);
    qr.make();
    qrEl.innerHTML = qr.createSvgTag({ cellSize: 8, margin: 4, scalable: true });

    // Next update exactly on the next second boundary of the corrected clock.
    setTimeout(render, 1000 - (now() % 1000) + 5);
  }

  sync();
  setInterval(sync, 5 * 60 * 1000);
  render();
})();
