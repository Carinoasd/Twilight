const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { test } = require("node:test");

const root = path.join(__dirname, "..");
const localeDir = path.join(root, "src/locales");
const load = (name) => JSON.parse(fs.readFileSync(path.join(localeDir, name), "utf8"));

function leaves(obj, prefix = "", out = {}) {
  for (const [key, value] of Object.entries(obj)) {
    const full = prefix ? `${prefix}.${key}` : key;
    if (value && typeof value === "object") leaves(value, full, out);
    else out[full] = value;
  }
  return out;
}
const placeholders = (text) => [...String(text).matchAll(/\{([a-zA-Z0-9_]+)\}/g)].map((m) => m[1]).sort().join(",");

// 与 check_placeholders.mjs 同口径：译文键在 basic.json 里找不到时按「无占位符」比较。
test("translated catalogs use the same placeholders as basic.json", () => {
  const basic = leaves(load("basic.json"));
  for (const name of ["zh-Hans.json", "zh-Hant.json", "en-US.json"]) {
    const catalog = leaves(load(name));
    for (const [key, value] of Object.entries(catalog)) {
      assert.equal(placeholders(value), placeholders(basic[key] ?? ""), `${name}: placeholder mismatch for ${key}`);
    }
  }
});

test("dashboard line counters are translated under dashboard.*", () => {
  for (const name of ["zh-Hant.json", "en-US.json"]) {
    const catalog = leaves(load(name));
    for (const key of ["dashboard.viewLines", "dashboard.publicLinesCount", "dashboard.dedicatedLinesCount"]) {
      assert.ok(catalog[key], `${name}: missing ${key}`);
    }
  }
});

test("user-facing error helpers no longer hardcode Chinese text", () => {
  const han = /[一-鿿]/;
  for (const file of ["src/lib/password.ts", "src/hooks/use-async-handler.ts", "src/app/error.tsx", "src/store/auth.ts"]) {
    const lines = fs.readFileSync(path.join(root, file), "utf8").split("\n");
    const offenders = lines
      .map((line, index) => ({ line, index }))
      .filter(({ line }) => {
        const code = line.replace(/\/\/.*$/, "").trim();
        if (!code || code.startsWith("*") || code.startsWith("/*")) return false;
        // friendlyError 的兜底哨兵值比较，不会展示给用户
        if (code.includes('friendly !== "操作失败"')) return false;
        return han.test(code);
      });
    assert.deepEqual(offenders.map(({ index, line }) => `${file}:${index + 1}: ${line.trim()}`), []);
  }
});

test("new i18n keys exist in all four catalogs", () => {
  const keys = ["passwordStrength.tooShort", "asyncHandler.status401", "errorPage.title", "authStore.sessionChanged", "adminAuditLog.filterSource"];
  for (const name of ["basic.json", "zh-Hans.json", "zh-Hant.json", "en-US.json"]) {
    const catalog = leaves(load(name));
    for (const key of keys) assert.ok(typeof catalog[key] === "string" && catalog[key], `${name}: missing ${key}`);
  }
});
