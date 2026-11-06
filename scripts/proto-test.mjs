// Speaks the DBX stdio-framed protocol to the sidecar binary and runs a
// smoke test over the JSON-RPC layer. Usage: node scripts/proto-test.mjs <exe>
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";

const exe = process.argv[2];
if (!exe) {
  console.error("usage: node proto-test.mjs <sidecar-exe>");
  process.exit(2);
}
// Isolate the data dir: without this the sidecar falls back to .dbx-dev/, whose
// cached specs make the lenient single-connection fallback silently dial the
// developer's REAL ssh server during session/newShell.
const dataDir = mkdtempSync(join(tmpdir(), "termhive-proto-"));
process.env.DBX_PLUGIN_DATA_DIR = dataDir;
const child = spawn(exe, [], { stdio: ["pipe", "pipe", "inherit"] });
const cleanup = () => {
  child.kill();
  rmSync(dataDir, { recursive: true, force: true });
};
process.on("exit", cleanup);

const pending = new Map();
let seq = 0;
let buffer = Buffer.alloc(0);

child.stdout.on("data", (chunk) => {
  buffer = Buffer.concat([buffer, chunk]);
  for (;;) {
    if (buffer.length < 5) return;
    const kind = buffer[0];
    const length = buffer.readUInt32BE(1);
    if (buffer.length < 5 + length) return;
    const payload = buffer.subarray(5, 5 + length);
    buffer = buffer.subarray(5 + length);
    if (kind === 0) {
      const message = JSON.parse(payload.toString("utf8"));
      if (message.id !== undefined && pending.has(message.id)) {
        pending.get(message.id)(message);
        pending.delete(message.id);
      } else if (message.method) {
        // notifications from the sidecar are not inspected here
      }
    } else if (kind === 1) {
      // binary frames are not inspected here; a crash on an unexpected one
      // would hide the real story, so just note the channel
      const channelLength = payload.readUInt16BE(0);
      const channel = payload.subarray(2, 2 + channelLength).toString("utf8");
      console.log(`  (binary frame on ${channel})`);
    }
  }
});

function frame(kind, payload) {
  const header = Buffer.alloc(5);
  header[0] = kind;
  header.writeUInt32BE(payload.length, 1);
  return Buffer.concat([header, payload]);
}

function call(method, params = null, timeoutMs = 15000) {
  const id = ++seq;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(id);
      reject(new Error(`timeout waiting for ${method}`));
    }, timeoutMs);
    pending.set(id, (message) => {
      clearTimeout(timer);
      if (message.error) reject(new Error(`${method}: ${message.error.message}`));
      else resolve(message.result);
    });
    const payload = Buffer.from(JSON.stringify({ jsonrpc: "2.0", id, method, params }));
    child.stdin.write(frame(0, payload));
  });
}

function expect(condition, label) {
  if (!condition) throw new Error(`assertion failed: ${label}`);
  console.log(`  ok ${label}`);
}

// lifecycle payload in the exact shape the host sends (see dev-host
// connections.mjs lifecyclePayload()).
function lifecycle(overrides = {}) {
  return {
    provider: { id: "com.milii.termhive.provider.ssh", databaseType: "ssh" },
    connection: {
      id: "conn-test-1",
      db_type: "plugin",
      plugin_id: "com.milii.termhive",
      plugin_connection_provider: "com.milii.termhive.provider.ssh",
      plugin_connection_type: "ssh",
      name: "test-host",
      host: "127.0.0.1",
      port: 22,
      username: "root",
      password: "secret",
      external_config: { auth_method: "password", encoding: "utf-8" },
      connection_secrets: {},
      read_only: false,
      query_timeout_secs: 60,
      idle_timeout_secs: 60,
    },
    runtime: null,
    ...overrides,
  };
}

try {
  const init = await call("plugin/initialize", { host: { protocolVersions: [1] } });
  expect(init.protocolVersion === 1, "initialize negotiates protocol v1");
  expect(init.plugin.id === "com.milii.termhive", "initialize reports plugin id");

  const ping = await call("plugin/ping");
  expect(ping.ok === true, "plugin/ping");

  const shells = await call("session/shells");
  expect(Array.isArray(shells) && shells.length === 0, "no live shells");

  const testFail = await call("connection/test", lifecycle({ connection: { ...lifecycle().connection, host: "127.0.0.1", port: 1 } })).then(
    () => {
      throw new Error("test should have failed");
    },
    (error) => error.message,
  );
  expect(/dial|connect|refused/i.test(testFail), `connection/test surfaces dial failure: ${testFail.slice(0, 50)}`);

  const connectFail = await call("connection/connect", lifecycle({ connection: { ...lifecycle().connection, port: 1 } })).then(
    () => {
      throw new Error("connect should have failed");
    },
    (error) => error.message,
  );
  expect(/dial|connect|refused/i.test(connectFail), "connection/connect surfaces dial failure");

  const ensureUnknown = await call("connection/ensure", { connectionId: "never-connected" }).then(
    () => {
      throw new Error("ensure should have failed");
    },
    (error) => error.message,
  );
  expect(/尚未建立|not/i.test(ensureUnknown), "connection/ensure rejects unknown connection");

  const newShellUnknown = await call("session/newShell", { connectionId: "never-connected", cols: 80, rows: 24 }).then(
    () => {
      throw new Error("newShell should have failed");
    },
    (error) => error.message,
  );
  expect(/尚未建立|not/i.test(newShellUnknown), "session/newShell rejects unknown connection");

  const badMethod = await call("fs/list", { shellId: "missing" }).then(
    () => {
      throw new Error("fs/list should have failed");
    },
    (error) => error.message,
  );
  expect(/not connected/.test(badMethod), "fs ops reject without a live shell");

  const badPayload = await call("connection/connect", { provider: { id: "x" }, connection: {} }).then(
    () => {
      throw new Error("malformed connect should have failed");
    },
    (error) => error.message,
  );
  expect(/no connection id/i.test(badPayload), "malformed lifecycle payload rejected");

  const hostKeys = await call("hostkey/list");
  expect(Array.isArray(hostKeys.hosts), "hostkey/list returns a list");
  const removedUnknown = await call("hostkey/remove", { host: "203.0.113.1", port: 22 });
  expect(removedUnknown.removed === false, "hostkey/remove of an unknown host reports false");

  const connList = await call("connection/list");
  expect(Array.isArray(connList.connections), "connection/list returns a list without credentials");

  const workspaceEmpty = await call("workspace/get");
  expect(workspaceEmpty.workspace && typeof workspaceEmpty.workspace === "object", "workspace/get returns an object");
  const layout = { v: 1, savedAt: Date.now(), tabs: [{ connectionId: "c1", title: "s1" }], groups: [] };
  await call("workspace/put", { workspace: layout });
  const workspaceBack = await call("workspace/get");
  expect(
    workspaceBack.workspace &&
      Array.isArray(workspaceBack.workspace.tabs) &&
      workspaceBack.workspace.tabs[0].title === "s1",
    "workspace/put → get round-trips the layout",
  );

  console.log("\nprotocol smoke test passed");
  child.kill();
  process.exit(0);
} catch (error) {
  console.error(`\nFAILED: ${error.message}`);
  child.kill();
  process.exit(1);
}
