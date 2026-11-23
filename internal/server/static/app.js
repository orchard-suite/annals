// The board's browser telemetry (Grafana Faro, relayed by the server as
// annals-web) and the copy button. Nothing here repaints on its own.
(function () {
  var sdk = window.GrafanaFaroWebSdk, tracing = window.GrafanaFaroWebTracing;
  var faro = null;
  if (sdk && typeof sdk.initializeFaro === 'function') {
    var instrumentations = sdk.getWebInstrumentations({ captureConsole: false, enablePerformanceInstrumentation: false });
    if (tracing && tracing.TracingInstrumentation) {
      instrumentations.push(new tracing.TracingInstrumentation({ instrumentationOptions: { fetchInstrumentationOptions: { ignoreUrls: [/\/api\/telemetry/] } } }));
    }
    try {
      faro = sdk.initializeFaro({
        url: '/api/telemetry',
        app: { name: 'annals-web', version: document.documentElement.dataset.version || 'dev' },
        ignoreUrls: [/\/api\/telemetry/],
        instrumentations: instrumentations,
        sessionTracking: { enabled: true, persistent: false }
      });
    } catch (e) { faro = null; }
  }
  function ready() {
    if (!faro) return;
    var ms = Math.round(performance.now());
    faro.api.pushMeasurement({ type: 'board_ready', values: { ready_ms: ms } });
    var page = document.body.dataset.page || location.pathname.split('/')[1] || 'today';
    faro.api.pushEvent('page.open', { page: page });
  }
  if (document.readyState === 'complete') ready(); else window.addEventListener('load', ready);

  var form = document.querySelector('[data-filter]');
  if (form) form.addEventListener('submit', function () {
    if (!faro) return;
    var set = [];
    ['actor', 'source', 'system', 'outcome', 'verb'].forEach(function (k) { if (form.elements[k] && form.elements[k].value) set.push(k); });
    faro.api.pushEvent('filter.apply', { fields: set.join(',') || 'none' });
  });

  document.addEventListener('click', function (e) {
    var b = e.target.closest('[data-copy]');
    if (!b) return;
    navigator.clipboard.writeText(b.getAttribute('data-copy')).then(function () {
      var t = b.textContent; b.textContent = 'Copied'; setTimeout(function () { b.textContent = t; }, 1200);
      if (faro) faro.api.pushEvent('token.copy', {});
    });
  });
})();
