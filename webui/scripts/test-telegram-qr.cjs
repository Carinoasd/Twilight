const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");
const source = fs.readFileSync(path.join(__dirname, "../src/components/telegram-qr.tsx"), "utf8");
const compiled = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020, jsx: ts.JsxEmit.ReactJSX },
}).outputText;
const exportsQR = {};
vm.runInNewContext(compiled, {
  exports: exportsQR, URL,
  require: (name) => name === "@/lib/i18n" ? {} : require(name),
});

test("QR accepts only bounded Telegram bot start links", () => {
  for (const token of ["abcdef0123456789abcdef0123456789", "login_abcdef0123456789abcdef0123456789"]) {
    const url = `https://t.me/twilight_test_bot?start=${token}`;
    assert.equal(exportsQR.telegramQRUrl(url), url);
  }
});

test("QR rejects external, credential-bearing, oversized and ambiguous links", () => {
  for (const url of ["javascript:alert(1)", "http://t.me/test_bot?start=abcdefgh", "https://evil.example/test_bot?start=abcdefgh", "https://user@t.me/test_bot?start=abcdefgh", "https://t.me/test_bot?start=abcdefgh&secret=hidden", "https://t.me/test_bot?start=abcdefgh&start=ijklmnop", "https://t.me/test_bot?start=abcdefgh#secret", `https://t.me/test_bot?start=${"a".repeat(65)}`, "https://t.me/test_bot?start=x"]) {
    assert.equal(exportsQR.telegramQRUrl(url), null, url);
  }
});
