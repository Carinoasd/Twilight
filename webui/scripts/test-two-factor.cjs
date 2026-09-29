const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");
const output = ts.transpileModule(fs.readFileSync(path.join(__dirname, "../src/lib/two-factor.ts"), "utf8"), { compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 } }).outputText;
const exported = {};
vm.runInNewContext(output, { exports: exported, Date });

test("second-step response requires a bounded live opaque credential", () => {
  const valid = { two_factor_required: true, request: "a".repeat(64), expires_at: Math.floor(Date.now()/1000)+180 };
  assert.equal(exported.readTwoFactorChallenge(valid).request, valid.request);
  for (const value of [null, {}, { ...valid, two_factor_required: false }, { ...valid, request: "short" }, { ...valid, request: "http://example.test/secret" }, { ...valid, expires_at: 0 }, { ...valid, expires_at: Infinity }, { ...valid, expires_at: valid.expires_at + 600 }]) {
    assert.equal(exported.readTwoFactorChallenge(value), null);
  }
});

test("second-step errors distinguish retriable bad codes, expiry and deployment failure", () => {
  assert.equal(exported.twoFactorErrorKey("AUTH_TWO_FACTOR_CODE_INVALID"), "twoFactor.invalidCode");
  assert.equal(exported.twoFactorErrorKey("AUTH_TWO_FACTOR_INVALID"), "twoFactor.expired");
  assert.equal(exported.twoFactorErrorKey("AUTH_TWO_FACTOR_UNAVAILABLE"), "twoFactor.unavailable");
  assert.equal(exported.twoFactorErrorKey("REQUEST_TIMEOUT"), "twoFactor.failed");
});
