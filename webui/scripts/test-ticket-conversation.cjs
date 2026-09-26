const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const { webcrypto } = require("node:crypto");
const ts = require("typescript");

function load(relative, requireModule = () => { throw new Error("Unexpected import"); }) {
  const compiled = ts.transpileModule(fs.readFileSync(path.join(__dirname, relative), "utf8"), {
    compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
  }).outputText;
  const exports = {};
  vm.runInNewContext(compiled, { exports, require: requireModule, crypto: webcrypto, Uint8Array });
  return exports;
}
const models = load("../src/lib/tickets.ts");

test("bounded detail preserves total and stable IDs rather than counting the current page", () => {
  const ticket = models.normalizeTicket({ id: 3, revision: 150, reply_count: 123, replies: [{ id: 74, uid: 2, role: 1, content: "owner" }], message_page: { has_more: true, next_before: 74, total: 123 } });
  assert.equal(ticket.reply_count, 123);
  assert.equal(ticket.replies[0].id, 74);
  assert.equal(ticket.message_page.next_before, 74);
  assert.equal(ticket.revision, 150);
});

test("unknown historical authors are not displayed as administrators", () => {
  assert.equal(models.normalizeTicketReply({ uid: 99, content: "legacy" }).is_admin, false);
  assert.equal(models.normalizeTicketReply({ uid: 99, role: -1, content: "legacy" }).is_admin, false);
  assert.equal(models.normalizeTicketReply({ uid: 1, role: 0, content: "admin" }).is_admin, true);
  assert.equal(models.normalizeTicketReply({ uid: 1, role: 0, is_admin: false }).is_admin, false);
});

test("late replies cannot overwrite another conversation or a newer revision", () => {
  const current = { id: 5, revision: 12, status: "closed" };
  assert.equal(models.mergeTicketResponse(current, { id: 4, revision: 13 }), current);
  assert.equal(models.mergeTicketResponse(current, { id: 5, revision: 11, status: "open" }), current);
  assert.equal(models.mergeTicketResponse(null, { id: 5, revision: 20 }), null);
  const latest = { id: 5, revision: 13 };
  assert.equal(models.mergeTicketResponse(current, latest), latest);
});

test("reply retries reuse the key; edits, ticket changes, and success retire it", () => {
  const { useTicketReplyKey } = load("../src/hooks/use-ticket-reply-key.ts", (name) => {
    assert.equal(name, "react");
    return { useRef: (value) => ({ current: value }), useCallback: (fn) => fn };
  });
  const { keyFor, retire } = useTicketReplyKey();
  const first = keyFor(1, "hello");
  assert.match(first, /^[a-f0-9]{48}$/);
  assert.equal(keyFor(1, "hello"), first);
  const edited = keyFor(1, "edited");
  assert.notEqual(edited, first);
  retire(first); // A late old request must not retire the new draft.
  assert.equal(keyFor(1, "edited"), edited);
  retire(edited);
  const again = keyFor(1, "edited");
  assert.notEqual(again, edited);
  assert.notEqual(keyFor(2, "edited"), again);
});
