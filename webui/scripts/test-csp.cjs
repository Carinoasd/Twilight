const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");

const source = fs.readFileSync(path.join(__dirname, "../src/lib/csp.ts"), "utf8");
const compiled = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
}).outputText;
const moduleExports = {};
vm.runInNewContext(compiled, { exports: moduleExports, URL });
const { buildContentSecurityPolicy } = moduleExports;

function directive(csp, name) {
  return csp.split("; ").find((part) => part.startsWith(`${name} `)) || "";
}

test("img-src is an allowlist instead of any https host", () => {
  const csp = buildContentSecurityPolicy({
    isDev: false,
    requestOrigin: "https://panel.example.com",
    apiUrl: "https://api.example.com/api",
    extraImg: "https://img-proxy.example.org/t/p * https: javascript:alert(1)",
  });
  const img = directive(csp, "img-src").split(" ");
  assert.ok(!img.includes("https:"), img.join(" "));
  assert.ok(!img.includes("*"), img.join(" "));
  for (const expected of ["'self'", "data:", "blob:", "https://api.example.com", "https://image.tmdb.org", "https://*.bgm.tv", "https://img-proxy.example.org"]) {
    assert.ok(img.includes(expected), `missing ${expected} in ${img.join(" ")}`);
  }
});

test("production script-src keeps self + unsafe-inline without eval", () => {
  const csp = buildContentSecurityPolicy({ isDev: false, requestOrigin: "https://panel.example.com" });
  assert.equal(directive(csp, "script-src"), "script-src 'self' 'unsafe-inline' https://static.cloudflareinsights.com");
  assert.match(directive(csp, "connect-src"), /wss:\/\/panel\.example\.com/);
});
