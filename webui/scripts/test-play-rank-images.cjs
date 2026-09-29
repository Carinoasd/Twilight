const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");
const source = fs.readFileSync(path.join(__dirname, "../src/lib/play-rank-image-url.ts"), "utf8");
const compiled = ts.transpileModule(source, { compilerOptions: { module: ts.ModuleKind.CommonJS } }).outputText;
const exportsUnderTest = {};
vm.runInNewContext(compiled, { exports: exportsUnderTest });
const { playRankImageUrl } = exportsUnderTest;

test("rank images use only the matching authenticated image resource", () => {
  const poster = "/api/v2/emby/items/series-123/image";
  const avatar = "/api/v2/emby/play-rank/avatars/0123456789abcdef.png";
  assert.equal(playRankImageUrl(poster, "poster"), poster);
  assert.equal(playRankImageUrl(avatar, "avatar"), avatar);
  assert.equal(playRankImageUrl(poster, "avatar"), undefined);
  assert.equal(playRankImageUrl(avatar, "poster"), undefined);
});

test("legacy, external, credential-bearing and traversal URLs cannot be loaded", () => {
  for (const value of [undefined, "", "https://example.org/avatar.png", "//example.org/avatar.png", "data:image/png;base64,AA==", "/api/v2/users/123/avatar", "/api/v1/users/assets/avatar/0123456789abcdef.png", "/api/v2/emby/items/../image", "/api/v2/emby/items/%2e%2e/image", "/api/v2/emby/items/123/image?api_key=secret", "/api/v2/emby/play-rank/avatars/0123456789abcdef.svg", "/api/v2/emby/play-rank/avatars/0123456789abcdef.png\n"]) {
    assert.equal(playRankImageUrl(value, "poster"), undefined, String(value));
    assert.equal(playRankImageUrl(value, "avatar"), undefined, String(value));
  }
});
