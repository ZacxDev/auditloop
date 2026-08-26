// Canonical source for the goal-directed walkthrough DRIVER's per-turn
// INTERACTIVE DIGEST (Phase 3). Returns a BOUNDED (<=40) list of the VISIBLE
// interactive elements on the current page — each with a best-effort CSS selector,
// its accessible name, tag/role/type, and disabled state — as a JSON string. The
// planner reads this so it can drive by REAL selectors rather than guessing.
//
// SECURITY: this runs read-only (it never mutates the page) and its OUTPUT is
// treated as UNTRUSTED — the selectors it emits round-trip ONLY through chromedp's
// selector engine, never back through eval. The whole body is try/caught so a
// hostile/exotic page degrades to an empty list rather than breaking the driver.
(() => {
 try {
  const MAX = 40;
  const out = [];
  const sel = (el) => {
    if (!el || el.nodeType !== 1) return '';
    if (el.id) return el.tagName.toLowerCase() + '#' + CSS.escape(el.id);
    const nm = el.getAttribute('name');
    if (nm) return el.tagName.toLowerCase() + '[name="' + nm.replace(/"/g, '\\"') + '"]';
    const cls = (el.className && typeof el.className === 'string')
      ? '.' + el.className.trim().split(/\s+/).slice(0, 2).map(CSS.escape).join('.') : '';
    return el.tagName.toLowerCase() + cls;
  };
  const visible = (el) => {
    const r = el.getBoundingClientRect();
    if (r.width === 0 && r.height === 0) return false;
    const st = getComputedStyle(el);
    return st.visibility !== 'hidden' && st.display !== 'none' && parseFloat(st.opacity || '1') > 0.01;
  };
  const nameOf = (el) => {
    const aria = el.getAttribute('aria-label');
    if (aria) return aria.trim();
    if (el.tagName === 'INPUT' || el.tagName === 'TEXTAREA' || el.tagName === 'SELECT') {
      const ph = el.getAttribute('placeholder');
      if (ph) return ph.trim();
      if (el.labels && el.labels.length && el.labels[0].textContent) return el.labels[0].textContent.trim();
      const al = el.getAttribute('aria-labelledby');
      if (al) { const l = document.getElementById(al); if (l) return (l.textContent || '').trim(); }
    }
    const txt = (el.textContent || '').trim().replace(/\s+/g, ' ');
    if (txt) return txt;
    const val = el.getAttribute('value');
    return val ? val.trim() : '';
  };
  const q = 'a[href], button, input, select, textarea, [role=button], [role=link], [role=tab], [role=checkbox], [role=menuitem], [contenteditable=true]';
  for (const el of document.querySelectorAll(q)) {
    if (out.length >= MAX) break;
    if (!visible(el)) continue;
    const type = el.getAttribute('type') || '';
    if (type === 'hidden') continue;
    out.push({
      tag: el.tagName.toLowerCase(),
      role: el.getAttribute('role') || '',
      name: nameOf(el).slice(0, 120),
      selector: sel(el).slice(0, 200),
      type: type,
      disabled: !!(el.disabled || el.getAttribute('aria-disabled') === 'true'),
    });
  }
  return JSON.stringify({ elements: out });
 } catch (e) {
  return '{"elements":[]}';
 }
})()
