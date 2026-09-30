(function () {
  'use strict';
  // Maps are declared in the HTML as <div class="map" data-lat data-lon> (one marker) or
  // <div class="map" data-json="/folders/ID/map.json?..."> (every geotagged photo of a filter).
  // Leaflet is vendored under /static/leaflet and only loaded when a map is on the page, so
  // no tile is requested until someone opens one.
  var BASE = '/static/leaflet/';
  var loading = null;

  function load(src, cb) {
    var s = document.createElement('script');
    s.src = src; s.onload = function () { cb(); }; s.onerror = function () { cb(new Error(src)); };
    document.head.appendChild(s);
  }

  function ensureLeaflet(cb) {
    if (window.L && window.L.markerClusterGroup) { cb(); return; }
    if (loading) { loading.push(cb); return; }
    loading = [cb];
    ['leaflet.css', 'MarkerCluster.css', 'MarkerCluster.Default.css'].forEach(function (f) {
      var l = document.createElement('link');
      l.rel = 'stylesheet'; l.href = BASE + f;
      document.head.appendChild(l);
    });
    load(BASE + 'leaflet.js', function (err) {
      if (err) {
        loading = null;
        document.querySelectorAll('.map[data-ready]').forEach(function (m) { m.removeAttribute('data-ready'); });
        return;
      }
      load(BASE + 'leaflet.markercluster.js', function () {
        window.L.Icon.Default.prototype.options.imagePath = BASE + 'images/';
        var cbs = loading; loading = null;
        cbs.forEach(function (f) { f(); });
      });
    });
  }

  function tiles(map) {
    window.L.tileLayer('https://tile.openstreetmap.org/{z}/{x}/{y}.png', {
      maxZoom: 19,
      // The OSM tile policy requires a Referer. The site-wide policy sends none
      // cross-origin, so tiles send just this site's origin (never a path).
      referrerPolicy: 'strict-origin-when-cross-origin',
      attribution: '&copy; <a href="https://www.openstreetmap.org/copyright">OpenStreetMap</a> contributors'
    }).addTo(map);
  }

  function init(el) {
    if (el.getAttribute('data-ready')) { return; }
    el.setAttribute('data-ready', '1');
    ensureLeaflet(function () {
      var L = window.L;
      el.innerHTML = '';
      var map = L.map(el);
      tiles(map);
      var json = el.getAttribute('data-json');
      if (!json) {
        var pos = [parseFloat(el.getAttribute('data-lat')), parseFloat(el.getAttribute('data-lon'))];
        map.setView(pos, 15);
        L.marker(pos).addTo(map);
        return;
      }
      map.setView([0, 0], 2);
      fetch(json, { credentials: 'same-origin', headers: { 'Accept': 'application/json' } })
        .then(function (r) { return r.json(); })
        .then(function (data) {
          var group = L.markerClusterGroup();
          (data.points || []).forEach(function (p) {
            var m = L.marker([p.lat, p.lon]);
            var a = document.createElement('a');
            a.href = '/images/' + p.id;
            a.setAttribute('hx-get', '/images/' + p.id);
            a.setAttribute('hx-target', '#modal');
            a.setAttribute('hx-swap', 'innerHTML');
            var img = document.createElement('img');
            img.src = '/images/' + p.id + '/thumbnail';
            img.alt = ''; img.width = 120;
            a.appendChild(img);
            m.bindPopup(a);
            m.on('popupopen', function () { if (window.htmx) { window.htmx.process(a); } });
            group.addLayer(m);
          });
          map.addLayer(group);
          if (data.points && data.points.length) { map.fitBounds(group.getBounds(), { maxZoom: 16, padding: [20, 20] }); }
        });
    });
  }

  function scan() { document.querySelectorAll('.map[data-lat], .map[data-json]').forEach(init); }
  document.addEventListener('DOMContentLoaded', scan);
  document.addEventListener('htmx:afterSwap', scan);
  document.addEventListener('htmx:afterSettle', scan);
  scan();
})();
