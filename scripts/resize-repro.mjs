// Repro for the resize rendering issue: drives the real sidecar against the
// local Windows sshd, then fires session/resize like a window drag would and
// dumps every binary frame that arrives on term/<shellId>. If the banner is
// re-emitted by the server on each window-change, we will see it in the raw
// stream; if not, the duplication is on our side.
// Usage: node scripts/resize-repro.mjs [keyPath]
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const exe = join(root, "backend", "bin", "dbx-plugin-termhive.exe");
const keyPath = process.argv[2] || join(homedir(), ".ssh", "id_ed25519");

const dataDir = mkdtempSync(join(tmpdir(), "termhive-repro-"));
const child = spawn(exe, [], { stdio: ["pipe", "pipe", "inherit"], env: { ...process.env, DBX_PLUGIN_DATA_DIR: dataDir } });

let seq = 0;
const pending = new Map();
let buffer = Buffer.alloc(0);
const frames = [];
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
      }
    } else if (kind === 1) {
      const channelLength = payload.readUInt16BE(0);
      const channel = payload.subarray(2, 2 + channelLength).toString("utf8");
      const data = payload.subarray(2 + channelLength);
      frames.push({ t: Date.now(), channel, data });
    }
  }
});

function frame(kind, payload) {
  const header = Buffer.alloc(5);
  header[0] = kind;
  header.writeUInt32BE(payload.length, 1);
  return Buffer.concat([header, payload]);
}
function call(method, params = null, timeoutMs = 30000) {
  const id = ++seq;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => reject(new Error(`timeout: ${method}`)), timeoutMs);
    pending.set(id, (message) => {
      clearTimeout(timer);
      message.error ? reject(new Error(`${method}: ${message.error.message}`)) : resolve(message.result);
    });
    child.stdin.write(frame(0, Buffer.from(JSON.stringify({ jsonrpc: "2.0", id, method, params }))));
  });
}
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function summarizeFrames(label) {
  const since = frames.filter((f) => f.channel.startsWith("term/"));
  console.log(`\n===== ${label}: ${since.length} term frames =====`);
  for (const [index, f] of since.entries()) {
    const text = f.data.toString("utf8");
    const banners = (text.match(/Microsoft Windows/g) || []).length;
    const clears = (text.match(/\x1b\[2J/g) || []).length;
    const homes = (text.match(/\x1b\[H/g) || []).length;
    const cup = (text.match(/\x1b\[\d+;\d+H/g) || []).length;
    const first = text.replace(/\x1b\[[0-9;?]*[a-zA-Z]/g, "").replace(/\r/g, "\\r").slice(0, 90);
    console.log(
      `frame[${index}] ${f.data.length}B banner×${banners} 2J×${clears} H×${homes} CUP×${cup} | ${first}`,
    );
  }
}

try {
  await call("plugin/initialize", { host: { protocolVersions: [1] } });
  const lifecycle = {
    provider: { id: "com.milii.termhive.provider.ssh", databaseType: "ssh" },
    connection: {
      id: "repro-1",
      db_type: "plugin",
      plugin_id: "com.milii.termhive",
      plugin_connection_provider: "com.milii.termhive.provider.ssh",
      plugin_connection_type: "ssh",
      name: "repro",
      host: "127.0.0.1",
      port: 22,
      username: process.env.USERNAME || "user",
      password: "",
      external_config: { auth_method: "key", key_path: keyPath, encoding: "utf-8" },
      connection_secrets: {},
    },
    runtime: null,
  };
  const connected = await call("connection/connect", lifecycle);
  console.log(`connected shell=${connected.shellId}`);
  const shellId = connected.shellId;
  await sleep(1500);
  frames.length = 0;
  summarizeFrames("initial output");

  // simulate a window drag: several resize steps with small pauses
  const steps = [
    { cols: 100, rows: 30 },
    { cols: 90, rows: 28 },
    { cols: 110, rows: 32 },
    { cols: 80, rows: 24 },
  ];
  for (const step of steps) {
    await call("session/resize", { shellId, ...step });
    await sleep(400);
  }
  await sleep(800);
  summarizeFrames("after 4 resize steps");

  // does a NO-OP resize (same dims) also trigger output?
  frames.length = 0;
  await call("session/resize", { shellId, cols: 80, rows: 24 });
  await sleep(600);
  summarizeFrames("after no-op resize (same dims)");

  child.kill();
  rmSync(dataDir, { recursive: true, force: true });
  process.exit(0);
} catch (error) {
  console.error(`FAILED: ${error.message}`);
  child.kill();
  rmSync(dataDir, { recursive: true, force: true });
  process.exit(1);
}
