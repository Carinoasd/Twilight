const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");
const { test } = require("node:test");
const ts = require("typescript");

// Execute the actual hook with a small effect/timer host. No browser or network
// is needed to exercise response ordering, terminal envelopes and cleanup.
const source = fs.readFileSync(
  path.join(__dirname, "../src/hooks/use-bind-code-status.ts"),
  "utf8",
);
const compiled = ts.transpileModule(source, {
  compilerOptions: { module: ts.ModuleKind.CommonJS, target: ts.ScriptTarget.ES2020 },
}).outputText;

function mount(read, overrides = {}) {
  const timers = new Map();
  const listeners = new Map();
  let nextTimer = 1;
  let cleanup;
  const bound = [];
  const failed = [];
  const exports = {};
  const context = {
    exports,
    require(name) {
      if (name === "react") return {
        useRef: (value) => ({ current: value }),
        useEffect: (effect) => { cleanup = effect(); },
      };
      if (name === "@/lib/api") return {
        api: { getRegisterBindCodeStatus: read, getBindCodeStatus: read },
      };
      throw new Error(`Unexpected import: ${name}`);
    },
    document: {
      visibilityState: "visible",
      addEventListener: (name, listener) => listeners.set(name, listener),
      removeEventListener: (name) => listeners.delete(name),
    },
    AbortController,
    setTimeout(callback, delay) {
      const id = nextTimer++;
      timers.set(id, { callback, delay });
      return id;
    },
    clearTimeout: (id) => timers.delete(id),
  };
  vm.runInNewContext(compiled, context);
  exports.useBindCodeStatus({
    code: "ABCDEF12",
    onBound: (data) => bound.push(data),
    onTerminalError: (data) => failed.push(data),
    ...overrides,
  });
  return { bound, failed, timers, listeners, cleanup: () => cleanup?.() };
}

const flush = () => new Promise((resolve) => setImmediate(resolve));

test("HTTP 200 success=false still delivers a terminal business failure", async () => {
  const state = mount(async () => ({
    success: false,
    data: { status: "expired", terminal: true, invalid: true },
  }));
  await flush();
  assert.equal(state.failed.length, 1);
  assert.equal(state.failed[0].status, "expired");
  assert.equal(state.bound.length, 0);
  assert.equal(state.timers.size, 0);
  state.cleanup();
});

test("invalid identity must not trigger the confirmed callback", async () => {
  const state = mount(async () => ({
    success: false,
    data: { status: "confirmed", confirmed: true, invalid: true, terminal: true },
  }));
  await flush();
  assert.equal(state.bound.length, 0);
  assert.equal(state.failed.length, 1);
  state.cleanup();
});

test("confirmed state stops polling and cancels the request", async () => {
  let signal;
  const state = mount(async (_code, requestSignal) => {
    signal = requestSignal;
    return { success: true, data: { status: "bound", telegram_bound: true, terminal: true } };
  });
  await flush();
  assert.equal(state.bound.length, 1);
  assert.equal(state.failed.length, 0);
  assert.equal(state.timers.size, 0);
  assert.equal(signal.aborted, true);
  state.cleanup();
});

test("cleanup aborts reads and ignores a late response", async () => {
  let resolve;
  let signal;
  const state = mount((_code, requestSignal) => {
    signal = requestSignal;
    return new Promise((done) => { resolve = done; });
  });
  state.cleanup();
  assert.equal(signal.aborted, true);
  resolve({ success: false, data: { status: "expired", terminal: true } });
  await flush();
  assert.equal(state.bound.length + state.failed.length, 0);
  assert.equal(state.timers.size, 0);
  assert.equal(state.listeners.size, 0);
});

test("pending continues polling while the overall deadline stays bounded", async () => {
  let timedOut = 0;
  const state = mount(async () => ({ success: true, data: { status: "pending" } }), {
    expiresIn: 10,
    onTimeout: () => { timedOut++; },
  });
  await flush();
  assert.equal(state.bound.length + state.failed.length, 0);
  assert.equal(state.timers.size, 2);
  const deadline = [...state.timers.values()].find((timer) => timer.delay === 15000);
  assert.ok(deadline);
  deadline.callback();
  assert.equal(timedOut, 1);
  assert.equal(state.timers.size, 0);
  state.cleanup();
});


test("retryable membership failure continues polling to confirmation", async () => {
  let attempts = 0;
  const state = mount(async () => ({ success: true, data: ++attempts === 1
    ? { status: "pending", terminal: false, error_code: "TG_BIND_GROUP_CHECK_FAILED" }
    : { status: "confirmed", terminal: true, confirmed: true } }), { scene: "register" });
  await flush();
  assert.equal(state.failed.length, 0);
  const poll = [...state.timers.values()].find((timer) => timer.delay === 2500);
  assert.ok(poll);
  poll.callback();
  await flush();
  assert.equal(state.bound.length, 1);
  assert.equal(state.failed.length, 0);
  state.cleanup();
});
