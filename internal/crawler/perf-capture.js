// Canonical source. External push harnesses (external ux-audit harnesses) copy this
// verbatim and must keep it in sync — the {lcp,cls,tbt} shape it returns feeds
// the deterministic perf metrics of the P5 push contract (report.Perf / the
// pages perf columns).
//
// Reads BUFFERED PerformanceObserver entries for the deterministic web-vitals lab
// signals and returns {lcp, cls, tbt} (ms/unitless/ms) as a JSON string. It observes
// with {buffered:true} so entries recorded before this eval ran are delivered, waits
// a short window for the observer callbacks to flush, then resolves. NOTE tbt is a
// headless LAB PROXY (max(0,longtask-50) summed) — there is no field input, so it
// approximates main-thread blocking, not real field TBT.
(() => new Promise((resolve) => {
  let lcp = 0, cls = 0, tbt = 0;
  const safeObserve = (type, cb) => {
    try { new PerformanceObserver(cb).observe({ type, buffered: true }); } catch (e) {}
  };
  safeObserve('largest-contentful-paint', (list) => {
    for (const e of list.getEntries()) lcp = Math.max(lcp, e.startTime);
  });
  safeObserve('layout-shift', (list) => {
    for (const e of list.getEntries()) if (!e.hadRecentInput) cls += e.value;
  });
  safeObserve('longtask', (list) => {
    for (const e of list.getEntries()) tbt += Math.max(0, e.duration - 50);
  });
  // Give the buffered observer callbacks a task to fire, then resolve.
  setTimeout(() => resolve(JSON.stringify({
    lcp: Math.round(lcp), cls: Math.round(cls * 10000) / 10000, tbt: Math.round(tbt),
  })), 250);
}))()
