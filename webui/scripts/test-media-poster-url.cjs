const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");

const source = fs.readFileSync(path.join(__dirname, "../src/lib/media-poster-url.ts"), "utf8");
const compiled = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
}).outputText;
const moduleExports = {};
vm.runInNewContext(compiled, { exports: moduleExports, URL });
const { sanitizeMediaPosterUrl } = moduleExports;

test("accepts TMDB / Bangumi posters and same-origin paths", () => {
  assert.equal(sanitizeMediaPosterUrl("https://image.tmdb.org/t/p/w500/a.jpg"), "https://image.tmdb.org/t/p/w500/a.jpg");
  assert.equal(sanitizeMediaPosterUrl("https://lain.bgm.tv/pic/cover/l/1.jpg"), "https://lain.bgm.tv/pic/cover/l/1.jpg");
  assert.equal(sanitizeMediaPosterUrl("/api/v2/bangumi/covers/1"), "/api/v2/bangumi/covers/1");
});

test("rejects attacker-controlled hosts and unsafe forms", () => {
  for (const value of [
    "https://attacker.example/p.png",
    "https://image.tmdb.org.attacker.example/p.png",
    "http://image.tmdb.org/t/p/w500/a.jpg",
    "https://user@image.tmdb.org/a.jpg",
    "https://image.tmdb.org:8443/a.jpg",
    "//attacker.example/p.png",
    "/\\attacker.example/p.png",
    "javascript:alert(1)",
    "data:image/png;base64,AAAA",
    "",
    null,
  ]) {
    assert.equal(sanitizeMediaPosterUrl(value), undefined, String(value));
  }
});
