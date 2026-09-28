const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");

function load(file) {
  const source = fs.readFileSync(path.join(__dirname, "..", file), "utf8");
  const compiled = ts.transpileModule(source, {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
  }).outputText;
  const moduleExports = {};
  vm.runInNewContext(compiled, { exports: moduleExports, URL, encodeURIComponent });
  return moduleExports;
}

const routes = load("src/lib/auth-routes.ts");
const events = load("src/lib/session-events.ts");

test("protected prefixes cover tickets, bangumi and playrank", () => {
  for (const page of ["/tickets", "/tickets/12", "/bangumi", "/playrank"]) {
    assert.equal(routes.safeProtectedRedirectTarget(page), page);
  }
});

test("login redirect carries next for protected pages only", () => {
  assert.equal(routes.buildLoginRedirect("/tickets/12", "?tab=open"), "/login?next=%2Ftickets%2F12%3Ftab%3Dopen");
  assert.equal(routes.buildLoginRedirect("/admin/users", ""), "/login?next=%2Fadmin%2Fusers");
  assert.equal(routes.buildLoginRedirect("/dashboard", ""), "/login");
  assert.equal(routes.buildLoginRedirect("//evil.example", ""), "/login");
});

test("any business 401 with UNAUTHORIZED is a session expiry, auth-flow 401s are not", () => {
  assert.equal(events.shouldTreatAsSessionExpired(401, "UNAUTHORIZED", "/tickets"), true);
  assert.equal(events.shouldTreatAsSessionExpired(401, undefined, "/admin/users?page=1"), true);
  assert.equal(events.shouldTreatAsSessionExpired(401, "AUTH_LOGIN_INVALID", "/auth/login"), false);
  assert.equal(events.shouldTreatAsSessionExpired(401, "UNAUTHORIZED", "/auth/login"), false);
  assert.equal(events.shouldTreatAsSessionExpired(403, "UNAUTHORIZED", "/tickets"), false);
});

test("session expired listeners are notified", () => {
  const seen = [];
  const off = events.onSessionExpired((info) => seen.push(info.endpoint));
  events.emitSessionExpired({ endpoint: "/tickets", status: 401 });
  off();
  events.emitSessionExpired({ endpoint: "/after-off", status: 401 });
  assert.deepEqual(seen, ["/tickets"]);
});

test("cross-tab identity changes reload or log out the stale tab", () => {
  assert.equal(events.decideCrossTabAction(5, { type: "identity", uid: 7 }), "reload");
  assert.equal(events.decideCrossTabAction(5, { type: "identity", uid: 5 }), "none");
  assert.equal(events.decideCrossTabAction(5, { type: "logout" }), "logout");
  assert.equal(events.decideCrossTabAction(null, { type: "identity", uid: 7 }), "none");
  assert.equal(events.isAuthChannelMessage({ type: "identity", uid: "7" }), false);
});
