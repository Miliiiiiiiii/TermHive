// Repro for the rc.15 leftover-fragment leak on 10.20.144.205 (linuxtest348d,
// CentOS 7.7 bash 4.2): drives the freshly built sidecar against the REAL
// server using the real sealed specs (same Windows account → DPAPI opens),
// mimics the UI's open sequence (ensure@80x24 → attach +130ms → resize
// +171ms), and dumps every term frame the UI would receive — the hide
// filter's output IS what lands here, so any leaked fragment shows up raw.
// Usage: node scripts/wrap-repro.mjs [connectionId] [holdSeconds]
import { spawn } from "node:child_process";
import { copyFileSync, mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const exe = join(root, "backend", "bin", "dbx-plugin-termhive.exe");
const realSpecs =
  process.env.APPDATA + "\\com.dbx.app\\plugin-data\\com.milii.termhive\\specs.json";
const targetId = process.argv[2] || "a39e3adc-0eb6-4d0a-b574-8b5ef9346cb6";
const holdSeconds = Number(process.argv[3] || 8);

const dataDir = mkdtempSync(join(tmpdir(), "termhive-wrap-"));
copyFileSync(realSpecs, join(dataDir, "specs.json"));

const child = spawn(exe, [], {
  stdio: ["pipe", "pipe", "inherit"],
  env: { ...process.env, DBX_PLUGIN_DATA_DIR: dataDir },
});

let seq = 0;
const pending = new Map();
let buffer = Buffer.alloc(0);
const frames = [];
const startedAt = Date.now();
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
      if (channel.startsWith("term/")) {
        frames.push({ t: Date.now() - startedAt, channel, data });
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
function call(method, params = null, timeoutMs = 30000) {
  const id = ++seq;
  return new Promise((resolve, reject) => {
    const timer = setTimeout(() => {
      pending.delete(id);
      reject(new Error(`timeout: ${method}`));
    }, timeoutMs);
    pending.set(id, (message) => {
      clearTimeout(timer);
      message.error ? reject(new Error(`${method}: ${message.error.message}`)) : resolve(message.result);
    });
    child.stdin.write(frame(0, Buffer.from(JSON.stringify({ jsonrpc: "2.0", id, method, params }))));
  });
}
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

const visualize = (buf) =>
  buf
    .toString("latin1")
    .replace(/\x1b\[[0-9;?]*[a-zA-Z]/g, (m) => `<${m.slice(1)}>`)
    .replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, "<OSC>")
    .replace(/\r/g, "<CR>")
    .replace(/\n/g, "<LF>\n");

try {
  const info = await call("connection/ensure", { connectionId: targetId });
  console.log(`shell ${info.shellId} on ${info.host}`);
  await sleep(130);
  await call("session/attach", { shellId: info.shellId });
  await sleep(41);
  await call("session/resize", { shellId: info.shellId, cols: 309, rows: 82 });
  await sleep(holdSeconds * 1000);

  const all = Buffer.concat(frames.map((f) => f.data));
  const text = all.toString("utf8");
  console.log(`\n=== ${frames.length} frames, ${all.length} bytes total ===`);
  for (const [index, f] of frames.entries()) {
    console.log(`frame[${index}] +${f.t}ms ${f.data.length}B | ${visualize(f.data).slice(0, 400)}`);
  }
  const dump = join(root, "wrap-stream-dump.txt");
  writeFileSync(dump, frames.map((f) => `+${f.t}ms ${f.data.length}B\n${visualize(f.data)}\n`).join("\n"));
  console.log(`\nfull dump: ${dump}`);
  console.log(`leak probes: LastLogin=${text.includes("Last login")} tail=${/from 172\./.test(text)}`);
} catch (error) {
  console.error("FAILED:", error.message);
} finally {
  child.kill();
  await sleep(200);
  rmSync(dataDir, { recursive: true, force: true });
  process.exit(0);
}
