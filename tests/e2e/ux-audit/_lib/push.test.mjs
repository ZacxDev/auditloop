// Tests for the opt-in auditloop push shim (_lib/push.ts).
// Run: node --test tests/e2e/ux-audit/_lib/*.test.mjs
// No new dependency: node:test + a local http.createServer fake. Node 24 imports
// the .ts module directly via native type-stripping.
import { test } from "node:test";
import assert from "node:assert/strict";
import * as http from "node:http";
import * as fs from "node:fs";
import * as os from "node:os";
import * as path from "node:path";
import { viewsToPushMetadata, pushRun } from "./push.ts";

// A 1x1 transparent PNG (smallest valid-ish blob; content is irrelevant here).
const PNG_1x1 = Buffer.from(
  "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+M9QDwADhgGAWjR9awAAAABJRU5ErkJggg==",
  "base64",
);

function makeView(overrides = {}) {
  return {
    slug: "view-a",
    title: "View A",
    route: "/a",
    screenshot: "01-view-a.png",
    console: [],
    network: [],
    axe: [],
    ...overrides,
  };
}

/** Build a throwaway runDir with the given screenshot filenames present. */
function makeRunDir(files) {
  const dir = fs.mkdtempSync(path.join(os.tmpdir(), "push-test-"));
  for (const f of files) fs.writeFileSync(path.join(dir, f), PNG_1x1);
  return dir;
}

/** Restore env after a test that mutates AUDITLOOP_* vars. */
function withEnv(vars, fn) {
  const saved = {};
  for (const k of Object.keys(vars)) saved[k] = process.env[k];
  Object.assign(process.env, vars);
  for (const [k, v] of Object.entries(vars)) if (v === undefined) delete process.env[k];
  return Promise.resolve()
    .then(fn)
    .finally(() => {
      for (const k of Object.keys(vars)) {
        if (saved[k] === undefined) delete process.env[k];
        else process.env[k] = saved[k];
      }
    });
}

// ---------------------------------------------------------------- mapper ----

test("mapper produces one page per view with slug as url + correct counts", () => {
  const views = [
    makeView({
      slug: "hub",
      screenshot: "01-hub.png",
      console: [{ severity: "error", text: "boom", location: "http://x/app.js:12" }],
      network: [{ status: 500, method: "GET", url: "http://x/api/y" }],
      axe: [
        { id: "color-contrast", impact: "serious", help: "Elements must have contrast", nodes: 3 },
        { id: "link-name", impact: "moderate", help: "Links need names", nodes: 1 },
      ],
    }),
    makeView({ slug: "login", screenshot: "02-login.png" }),
  ];

  const meta = viewsToPushMetadata(views, "core-pages 2026-07-08T00-00-00");
  assert.equal(meta.label, "core-pages 2026-07-08T00-00-00");
  assert.equal(meta.pages.length, 2);

  const hub = meta.pages[0];
  assert.equal(hub.url, "hub"); // url === slug
  assert.equal(hub.viewport, "desktop");
  assert.equal(hub.screenshot, "01-hub.png");
  assert.equal(hub.axe_violations, 4); // sum of nodes (3 + 1)
  assert.equal(hub.console_first_party, 1);
  assert.equal(hub.console_third_party, 0);
  assert.equal(hub.network_first_party, 1);
  assert.equal(hub.network_third_party, 0);

  // findings: a11y (2) + console (1) + network (1) = 4, correctly typed/mapped.
  assert.equal(hub.findings.length, 4);
  const a11y = hub.findings.filter((f) => f.type === "a11y");
  assert.equal(a11y.length, 2);
  assert.equal(a11y[0].severity, "serious");
  assert.match(a11y[0].detail, /color-contrast — Elements must have contrast/);
  const con = hub.findings.find((f) => f.type === "console");
  assert.equal(con.severity, "error");
  assert.match(con.detail, /boom \(http:\/\/x\/app\.js:12\)/);
  const net = hub.findings.find((f) => f.type === "network");
  assert.equal(net.detail, "500 GET http://x/api/y");

  // Clean view → zero counts, no findings.
  assert.equal(meta.pages[1].axe_violations, 0);
  assert.equal(meta.pages[1].findings.length, 0);
});

test("mapper skips views without a screenshot file (defensive)", () => {
  const meta = viewsToPushMetadata(
    [makeView({ slug: "ok", screenshot: "01-ok.png" }), makeView({ slug: "bail", screenshot: "" })],
    "x",
  );
  assert.equal(meta.pages.length, 1);
  assert.equal(meta.pages[0].url, "ok");
});

test("mapper emits ONLY the strict-schema keys (no unknown fields)", () => {
  const meta = viewsToPushMetadata([makeView()], "x");
  assert.deepEqual(Object.keys(meta).sort(), ["label", "pages"]);
  assert.deepEqual(
    Object.keys(meta.pages[0]).sort(),
    [
      "axe_violations",
      "console_first_party",
      "console_third_party",
      "findings",
      "network_first_party",
      "network_third_party",
      "screenshot",
      "url",
      "viewport",
    ].sort(),
  );
});

// --------------------------------------------------------------- pushRun ----

test("pushRun happy path: server gets metadata + one part per page + bearer", async () => {
  const runDir = makeRunDir(["01-a.png", "02-b.png"]);
  const audit = {
    stamp: "2026-07-08T00-00-00",
    runDir,
    findings: [
      makeView({ slug: "a", screenshot: "01-a.png" }),
      makeView({ slug: "b", screenshot: "02-b.png" }),
    ],
  };

  const received = { auth: "", meta: null, fileFields: [], contentType: "" };
  const server = http.createServer((req, res) => {
    received.auth = req.headers.authorization ?? "";
    received.contentType = req.headers["content-type"] ?? "";
    const chunks = [];
    req.on("data", (c) => chunks.push(c));
    req.on("end", () => {
      const raw = Buffer.concat(chunks).toString("latin1");
      // Crude multipart scrape (no dep): pull each part's form-field name.
      for (const m of raw.matchAll(/name="([^"]+)"/g)) received.fileFields.push(m[1]);
      const metaMatch = raw.match(/name="metadata"\r\n\r\n([\s\S]*?)\r\n--/);
      if (metaMatch) received.meta = JSON.parse(metaMatch[1]);
      res.writeHead(200, { "content-type": "application/json" });
      res.end(JSON.stringify({ run_id: "run-123", url: "http://auditloop/runs/run-123" }));
    });
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const base = `http://127.0.0.1:${server.address().port}`;

  await withEnv(
    { AUDITLOOP_PUSH_URL: base, AUDITLOOP_PUSH_TOKENS: JSON.stringify({ "core-pages": "tok-abc" }) },
    () => pushRun(audit, "core-pages"),
  );
  await new Promise((r) => server.close(r));
  fs.rmSync(runDir, { recursive: true, force: true });

  assert.equal(received.auth, "Bearer tok-abc");
  assert.match(received.contentType, /multipart\/form-data/);
  assert.ok(received.meta, "server parsed the metadata part");
  assert.equal(received.meta.label, "core-pages 2026-07-08T00-00-00");
  assert.equal(received.meta.pages.length, 2);
  // A metadata field + one field per screenshot, named by filename.
  assert.ok(received.fileFields.includes("metadata"));
  assert.ok(received.fileFields.includes("01-a.png"));
  assert.ok(received.fileFields.includes("02-b.png"));
});

test("pushRun is NON-FATAL on a 500 response (resolves, does not throw)", async () => {
  const runDir = makeRunDir(["01-a.png"]);
  const audit = { stamp: "s", runDir, findings: [makeView({ slug: "a", screenshot: "01-a.png" })] };
  const server = http.createServer((_req, res) => {
    res.writeHead(500);
    res.end("boom");
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const base = `http://127.0.0.1:${server.address().port}`;

  await assert.doesNotReject(() =>
    withEnv(
      { AUDITLOOP_PUSH_URL: base, AUDITLOOP_PUSH_TOKENS: JSON.stringify({ outreach: "t" }) },
      () => pushRun(audit, "outreach"),
    ),
  );
  await new Promise((r) => server.close(r));
  fs.rmSync(runDir, { recursive: true, force: true });
});

test("pushRun is NON-FATAL on connection refused (resolves, does not throw)", async () => {
  const runDir = makeRunDir(["01-a.png"]);
  const audit = { stamp: "s", runDir, findings: [makeView({ slug: "a", screenshot: "01-a.png" })] };
  // Port 1 is not listening → connection refused.
  await assert.doesNotReject(() =>
    withEnv(
      { AUDITLOOP_PUSH_URL: "http://127.0.0.1:1", AUDITLOOP_PUSH_TOKENS: JSON.stringify({ outreach: "t" }) },
      () => pushRun(audit, "outreach"),
    ),
  );
  fs.rmSync(runDir, { recursive: true, force: true });
});

test("pushRun with NO env resolves without hitting any server", async () => {
  const runDir = makeRunDir(["01-a.png"]);
  const audit = { stamp: "s", runDir, findings: [makeView({ slug: "a", screenshot: "01-a.png" })] };
  await assert.doesNotReject(() =>
    withEnv({ AUDITLOOP_PUSH_URL: undefined, AUDITLOOP_PUSH_TOKENS: undefined }, () =>
      pushRun(audit, "sales-audit"),
    ),
  );
  fs.rmSync(runDir, { recursive: true, force: true });
});

test("pushRun skips a spec whose token is absent from AUDITLOOP_PUSH_TOKENS", async () => {
  const runDir = makeRunDir(["01-a.png"]);
  const audit = { stamp: "s", runDir, findings: [makeView({ slug: "a", screenshot: "01-a.png" })] };
  let hit = false;
  const server = http.createServer((_req, res) => {
    hit = true;
    res.writeHead(200, { "content-type": "application/json" });
    res.end("{}");
  });
  await new Promise((r) => server.listen(0, "127.0.0.1", r));
  const base = `http://127.0.0.1:${server.address().port}`;
  await withEnv(
    { AUDITLOOP_PUSH_URL: base, AUDITLOOP_PUSH_TOKENS: JSON.stringify({ outreach: "t" }) },
    () => pushRun(audit, "sales-audit"), // no token for sales-audit
  );
  await new Promise((r) => server.close(r));
  fs.rmSync(runDir, { recursive: true, force: true });
  assert.equal(hit, false, "server must not be hit when this spec has no token");
});
