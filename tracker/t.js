/*!
 * Omega Analytics tracker.
 * <script defer src="https://YOUR-OMEGA-HOST/t.js" data-site="site_xxx"></script>
 *
 * Optional attributes:
 *   data-cookies="consent"   cookieless until omega.consent(true), then a first-party cookie
 *   data-cookies="always"    always use a first-party cookie
 *                            (leave it off for cookieless: nothing is stored in the browser)
 *   data-api="https://..."   where to send data (defaults to the script's origin)
 *   data-cookie-domain="example.com"  share the cookie across subdomains
 *
 * API:
 *   omega.track('Signup', { plan: 'pro' })
 *   omega.identify('user-123', { email: 'a@b.com', name: 'Ada' })
 *   omega.consent(true | false)                 // with data-cookies="consent"
 *   localStorage.setItem('omega_ignore', '1')   // stop tracking your own visits in this browser
 */
(function () {
  'use strict';
  var script = document.currentScript;
  if (!script || window.__omegaLoaded) return;
  var key = script.getAttribute('data-site');
  if (!key) return console.warn('[omega] Missing data-site attribute on the tracking script.');
  window.__omegaLoaded = true;

  var api = (script.getAttribute('data-api') || new URL(script.src).origin).replace(/\/$/, '') + '/api/collect';
  var cookieMode = script.getAttribute('data-cookies') || 'none'; // none | consent | always
  var cookieDomain = script.getAttribute('data-cookie-domain');
  var suffix = key.slice(-6);
  var VISITOR_COOKIE = '_oa_v_' + suffix;
  var SESSION_COOKIE = '_oa_s_' + suffix;
  var SESSION_MS = 30 * 60 * 1000;
  var PING_MS = 15000;

  var queued = window.omega && window.omega.q ? window.omega.q : [];
  var ignored = false;
  try { ignored = localStorage.getItem('omega_ignore') === '1'; } catch (e) {}
  if (ignored || navigator.webdriver) {
    var noop = function () {};
    window.omega = { track: noop, identify: noop, consent: noop };
    return;
  }

  function uid() {
    var bytes = new Uint8Array(12);
    crypto.getRandomValues(bytes);
    var s = '';
    for (var i = 0; i < bytes.length; i++) s += String.fromCharCode(bytes[i]);
    return btoa(s).replace(/\+/g, '-').replace(/\//g, '_').replace(/=+$/, '');
  }

  function readCookie(name) {
    var m = document.cookie.match('(?:^|; )' + name + '=([^;]*)');
    return m ? decodeURIComponent(m[1]) : null;
  }

  function writeCookie(name, value, seconds) {
    document.cookie = name + '=' + encodeURIComponent(value) + '; Max-Age=' + seconds + '; Path=/; SameSite=Lax' +
      (cookieDomain ? '; Domain=' + cookieDomain : '') + (location.protocol === 'https:' ? '; Secure' : '');
  }

  // ---- Identity ----
  // Cookieless (default): nothing is stored; the server derives a daily-rotating id.
  // Cookie modes: a first-party visitor cookie (~400 days) and a 30-minute rolling session cookie.
  // In "consent" mode an existing visitor cookie means the visitor accepted earlier.
  var cookiesOn = cookieMode === 'always' || (cookieMode === 'consent' && !!readCookie(VISITOR_COOKIE));
  var visitorId = null;

  function startCookies() {
    cookiesOn = true;
    visitorId = readCookie(VISITOR_COOKIE) || uid();
    writeCookie(VISITOR_COOKIE, visitorId, 400 * 86400);
  }
  if (cookiesOn) startCookies();

  function sessionId() {
    var sid = readCookie(SESSION_COOKIE) || uid();
    writeCookie(SESSION_COOKIE, sid, SESSION_MS / 1000);
    return sid;
  }

  // ---- Sending ----
  var pageviewId = null;
  var lastUrl = null;
  var lastHitAt = 0;

  function sessionExpired() {
    return cookiesOn ? !readCookie(SESSION_COOKIE) : Date.now() - lastHitAt > SESSION_MS;
  }

  function send(payload) {
    // The session timed out while the page sat open: start a new one with a fresh page view first.
    if (lastUrl && sessionExpired() && (payload.t === 'ping' || payload.t === 'event' || payload.t === 'identify')) {
      lastUrl = null;
      pageviewId = null;
      pageview();
      if (payload.t === 'ping') return;
      payload.p = pageviewId;
    }
    payload.k = key;
    if (cookiesOn) {
      payload.v = visitorId;
      payload.s = sessionId();
    }
    lastHitAt = Date.now();
    var body = JSON.stringify(payload);
    // A string body is sent as text/plain, which avoids a CORS preflight.
    if (navigator.sendBeacon && navigator.sendBeacon(api, body)) return;
    try { fetch(api, { method: 'POST', body: body, keepalive: true, mode: 'cors', credentials: 'omit' }); } catch (e) {}
  }

  // ---- Page views and engaged time ----
  var engagedMs = 0;
  var visibleSince = document.visibilityState === 'visible' ? Date.now() : null;

  function engaged() {
    return engagedMs + (visibleSince ? Date.now() - visibleSince : 0);
  }

  function pageview() {
    var url = location.href.split('#')[0];
    if (url === lastUrl) return;
    var previous = pageviewId;
    if (previous) send({ t: 'ping', p: previous, e: engaged() });
    var referrer = lastUrl || document.referrer;
    lastUrl = url;
    pageviewId = uid();
    engagedMs = 0;
    visibleSince = document.visibilityState === 'visible' ? Date.now() : null;
    send({
      t: 'pageview', p: pageviewId, pp: previous || undefined, u: url, r: referrer, ti: document.title,
      sw: screen.width, sh: screen.height, l: navigator.language,
      tz: Intl.DateTimeFormat().resolvedOptions().timeZone,
    });
  }

  // Single-page apps: count route changes as page views.
  ['pushState', 'replaceState'].forEach(function (fn) {
    var original = history[fn];
    history[fn] = function () {
      var out = original.apply(this, arguments);
      setTimeout(pageview, 0);
      return out;
    };
  });
  addEventListener('popstate', function () { setTimeout(pageview, 0); });

  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'visible') {
      visibleSince = Date.now();
      if (pageviewId) send({ t: 'ping', p: pageviewId, e: engaged() });
    } else if (visibleSince) {
      engagedMs += Date.now() - visibleSince;
      visibleSince = null;
      if (pageviewId) send({ t: 'ping', p: pageviewId, e: engaged() });
    }
  });

  addEventListener('pagehide', function () {
    if (pageviewId) send({ t: 'leave', p: pageviewId, e: engaged() });
  });

  // Heartbeat keeps the visitor on the live view and measures time on page.
  setInterval(function () {
    if (pageviewId && document.visibilityState === 'visible') send({ t: 'ping', p: pageviewId, e: engaged() });
  }, PING_MS);

  // ---- Public API ----
  window.omega = {
    track: function (name, props) {
      if (typeof name !== 'string' || !name) return;
      send({ t: 'event', p: pageviewId, en: name, ep: props || null, u: location.href });
    },
    identify: function (userId, traits) {
      if (userId === undefined || userId === null || userId === '') return;
      send({ t: 'identify', p: pageviewId, uid: String(userId), tr: traits || null });
    },
    consent: function (granted) {
      if (cookieMode !== 'consent') {
        return console.warn('[omega] consent() only applies with data-cookies="consent" on the tracking script.');
      }
      if (granted) {
        if (cookiesOn) return;
        startCookies();
        // Hand the current cookieless visit over to the cookie so it isn't counted twice.
        if (pageviewId) send({ t: 'consent', p: pageviewId });
      } else {
        cookiesOn = false;
        visitorId = null;
        writeCookie(VISITOR_COOKIE, '', 0);
        writeCookie(SESSION_COOKIE, '', 0);
      }
    },
  };

  // Elements with data-omega-event="Name" are tracked on click, with other data-omega-* attributes as props.
  document.addEventListener('click', function (e) {
    var el = e.target && e.target.closest ? e.target.closest('[data-omega-event]') : null;
    if (!el) return;
    var props = {};
    for (var i = 0; i < el.attributes.length; i++) {
      var a = el.attributes[i];
      if (a.name.indexOf('data-omega-') === 0 && a.name !== 'data-omega-event') props[a.name.slice(11)] = a.value;
    }
    window.omega.track(el.getAttribute('data-omega-event'), props);
  }, true);

  pageview();
  queued.forEach(function (call) { window.omega[call[0]] && window.omega[call[0]].apply(null, call[1]); });
})();
