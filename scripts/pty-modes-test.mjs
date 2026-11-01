// PTY modes regression test: connects the real sidecar to a paramiko server
// (tools/ptyserver) that records the pty-req terminal modes, then asserts
// the modes are what a POSIX line discipline expects. IGNCR must never be
// requested: on Linux it makes the tty discard the CR that xterm sends for
// Enter, so commands never execute (Windows OpenSSH ignores it, which is
// why the bug only showed against Linux targets).
// Requires python3 + paramiko on PATH. Usage: node scripts/pty-modes-test.mjs [exe]
import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const exe = process.argv[2] || join(root, "backend", "bin", "dbx-plugin-termhive.exe");
const modesFile = join(root, "tools", "ptyserver", "pty-modes.json");

function fail(message) {
  console.error(`FAILED: ${message}`);
  process.exit(1);
}

const ptyServer = spawn("python", [join(root, "tools", "ptyserver", "pty_modes_server.py")], {
  stdio: ["ignore", "pipe", "inherit"],
});
ptyServer.on("error", (error) => fail(`python/paramiko unavailable: ${error.message}`));

const readyLine = await new Promise((resolve, reject) => {
  const timer = setTimeout(() => reject(new Error("ptyserver timeout")), 20000);
  ptyServer.stdout.on("data", (chunk) => {
    const match = chunk.toString().match(/READY (\d+)/);
    if (match) {
      clearTimeout(timer);
      resolve(match[1]);
    }
  });
});

const dataDir = mkdtempSync(join(tmpdir(), "dbx-pty-modes-"));
const sidecar = spawn(exe, [], {
  stdio: ["pipe", "pipe", "inherit"],
  env: { ...process.env, DBX_PLUGIN_DATA_DIR: dataDir },
});

let seq = 0;
const pending = new Map();
let buffer = Buffer.alloc(0);
sidecar.stdout.on("data", (chunk) => {
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
      }
    }
  }
});

function frame(kind, payload) {
  const header = Buffer.alloc(5);
  header[0] = kind;
  header.writeUInt32BE(payload.length, 1);
  return Buffer.concat([header, payload]);
}
function call(method, params = null, timeoutMs = 20000) {
  const id = ++seq;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(id);
      reject(new Error(`timeout waiting for ${method}`));
    }, timeoutMs);
    pending.set(id, (message) => {
      clearTimeout(timer);
      message.error ? reject(new Error(`${method}: ${message.error.message}`)) : resolve(message.result);
    });
    sidecar.stdin.write(frame(0, Buffer.from(JSON.stringify({ jsonrpc: "2.0", id, method, params }))));
  });
}
function expect(condition, label) {
  if (!condition) throw new Error(`assertion failed: ${label}`);
  console.log(`  ok ${label}`);
}

try {
  await call("plugin/initialize", { host: { protocolVersions: [1] } });
  const connected = await call("connection/connect", {
    provider: { id: "com.milii.termhive.provider.ssh", databaseType: "ssh" },
    connection: {
      id: "conn-pty-modes",
      db_type: "plugin",
      plugin_id: "com.milii.termhive",
      plugin_connection_provider: "com.milii.termhive.provider.ssh",
      plugin_connection_type: "ssh",
      name: "pty-modes",
      host: "127.0.0.1",
      port: Number(readyLine),
      username: "tester",
      password: "pw123",
      external_config: { auth_method: "password", encoding: "utf-8" },
      connection_secrets: {},
    },
    runtime: null,
  });
  expect(Boolean(connected.shellId), "sidecar opened a PTY against the capture server");

  for (let waited = 0; waited < 100; waited++) {
    try {
      readFileSync(modesFile);
      break;
    } catch {
      await new Promise((resolve) => setTimeout(resolve, 100));
    }
  }
  const modes = JSON.parse(readFileSync(modesFile, "utf8")).parsed;

  expect(modes["35"] === undefined, "IGNCR is NOT requested (Linux tty would discard every Enter CR)");
  expect(modes["53"] === 1, "ECHO requested (opcode 53 = 1)");
  console.log(`  pty modes: ${JSON.stringify(modes)}`);

  console.log("\npty-modes test passed");
  sidecar.kill();
  ptyServer.kill();
  rmSync(dataDir, { recursive: true, force: true });
  process.exit(0);
} catch (error) {
  console.error(`\nFAILED: ${error.message}`);
  sidecar.kill();
  ptyServer.kill();
  rmSync(dataDir, { recursive: true, force: true });
  process.exit(1);
}
