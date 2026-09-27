/*
 * Omega Analytics session replay recorder. Served at /replay.js after rrweb's recorder (MIT,
 * https://github.com/rrweb-io/rrweb), and loaded by t.js only when the server says this session is recorded.
 *
 * Privacy: everything typed into form fields is masked. Elements with data-omega-mask have their text masked,
 * and elements with data-omega-block are left out (recorded as an empty box of the same size).
 *
 * Events are sent every few seconds as gzip-compressed NDJSON. A recording is this page load; a new session in
 * the same tab starts a new recording.
 */
(function (rrweb) {
  'use strict';
  var ctl = window.__omegaReplay;
  try { delete window.__omegaReplay; } catch (e) { window.__omegaReplay = undefined; }
  if (!ctl || ctl.recorder || !rrweb || !rrweb.record) return;

  var FLUSH_MS = 5000;
  var FLUSH_CHARS = 256 * 1024;
  var KEEPALIVE_MAX = 60 * 1024; // browsers cap keepalive requests at 64 KB in total

  var stopRecording = null;
  var session = -1;      // which of this tab's sessions is being recorded
  var refused = -1;      // the server said stop for this session (too long, or no longer recorded)
  var recordingId = null;
  var seq = 0;
  var buffer = [];
  var chars = 0;
  var timer = null;

  function gzip(text) {
    var stream = new Blob([text]).stream().pipeThrough(new CompressionStream('gzip'));
    return new Response(stream).blob();
  }

  function flush(leaving) {
    if (!buffer.length || !recordingId) return;
    var text = buffer.join('\n') + '\n';
    buffer = [];
    chars = 0;
    var rid = recordingId;
    var forSession = session;
    var url = ctl.api + '?k=' + encodeURIComponent(ctl.key) + '&p=' + encodeURIComponent(ctl.page() || '') +
      '&r=' + rid + '&n=' + (seq++);
    gzip(text).then(function (blob) {
      return fetch(url, {
        method: 'POST', body: blob, mode: 'cors', credentials: 'omit',
        keepalive: !!leaving && blob.size <= KEEPALIVE_MAX,
      });
    }).then(function (res) {
      // 200 means "stop": this session isn't recorded any more, or the recording hit its limit.
      if (res.status === 200 && rid === recordingId) {
        refused = forSession;
        stop();
      }
    }).catch(function () {});
  }

  function start() {
    stop();
    session = ctl.session;
    recordingId = ctl.uid();
    seq = 0;
    stopRecording = rrweb.record({
      emit: function (event) {
        var line = JSON.stringify(event);
        buffer.push(line);
        chars += line.length;
        if (chars >= FLUSH_CHARS) flush(false);
      },
      maskAllInputs: true,
      maskInputOptions: { password: true },
      maskTextSelector: '[data-omega-mask]',
      blockSelector: '[data-omega-block]',
      ignoreClass: 'omega-ignore',
      recordCanvas: false,
      collectFonts: false,
      inlineImages: false,
      sampling: { mousemove: 50, scroll: 150, media: 800, input: 'last' },
      slimDOMOptions: {
        script: true, comment: true, headFavicon: true, headWhitespace: true, headMetaSocial: true,
        headMetaRobots: true, headMetaHttpEquiv: true, headMetaAuthorship: true, headMetaVerification: true,
      },
    }) || null;
    timer = setInterval(function () { flush(false); }, FLUSH_MS);
  }

  function stop() {
    if (!stopRecording) return;
    stopRecording();
    stopRecording = null;
    clearInterval(timer);
    flush(false);
    recordingId = null;
  }

  // t.js calls sync() whenever the server answers a page view: keep recording, start a new recording for a
  // new session, or stop.
  function sync() {
    if (ctl.want && ctl.session !== refused) {
      if (!stopRecording || session !== ctl.session) start();
    } else {
      stop();
    }
  }

  document.addEventListener('visibilitychange', function () {
    if (document.visibilityState === 'hidden') flush(true);
  });
  addEventListener('pagehide', function () { flush(true); });

  ctl.recorder = { sync: sync };
  sync();
})(rrwebRecord);
