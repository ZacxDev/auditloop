package crawler

import _ "embed"

// axeSource is the vendored axe-core library, injected into each page to run an
// accessibility scan. Version is tracked in axe.min.js's banner (v4.12.1).
//
//go:embed axe.min.js
var axeSource string

// AxeVersion is the vendored axe-core version (kept in sync with axe.min.js).
const AxeVersion = "4.12.1"

// axeRunScript is evaluated (await-promise) after axeSource is injected. It runs
// axe against the whole document and returns the results as a JSON string. We
// keep the full violations payload (id, impact, help, nodes) so findings and the
// report.json carry actionable detail.
const axeRunScript = `
(async () => {
  try {
    const r = await axe.run(document, { resultTypes: ['violations'] });
    return JSON.stringify({
      violations: r.violations.map(v => ({
        id: v.id, impact: v.impact, help: v.help, helpUrl: v.helpUrl,
        tags: v.tags, nodeCount: v.nodes.length,
        nodes: v.nodes.slice(0, 5).map(n => ({ target: n.target, html: (n.html||'').slice(0,300) }))
      }))
    });
  } catch (e) {
    return JSON.stringify({ error: String(e), violations: [] });
  }
})()
`
