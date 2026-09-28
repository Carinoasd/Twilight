const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const { test } = require("node:test");

// 后端从不签发 status_token，/registration/emby/queue-status 与 /me/use-code/status
// 只是恒返回 terminal:true 的兼容桩；前端的排队轮询是死代码，已删除，防止回归。
function walk(dir, out = []) {
  for (const entry of fs.readdirSync(dir, { withFileTypes: true })) {
    const full = path.join(dir, entry.name);
    if (entry.isDirectory()) walk(full, out);
    else if (/\.(ts|tsx)$/.test(entry.name)) out.push(full);
  }
  return out;
}

test("webui no longer polls the stub queue-status endpoints", () => {
  const src = path.join(__dirname, "../src");
  const offenders = [];
  for (const file of walk(src)) {
    const text = fs.readFileSync(file, "utf8");
    for (const needle of ["queue-status", "use-code/status", "getUseCodeStatus", "getEmbyRegisterStatus"]) {
      if (text.includes(needle)) offenders.push(`${path.relative(src, file)}: ${needle}`);
    }
  }
  assert.deepEqual(offenders, []);
});
