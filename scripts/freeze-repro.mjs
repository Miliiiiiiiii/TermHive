// Repro for the intermittent "blank terminal, typing dead" on connect:
// drives the real sidecar against the local Windows sshd, mimicking the UI's
// exact open sequence (connect @80x24 -> attach ~+130ms -> resize to the
// fitted size ~+171ms -> probe keystrokes), in a loop of fresh connections
// and sequential shells, and classifies every run from the raw term stream.
//
// Healthy run: banner + prompt visible, probe echo + output arrive promptly.
// Stuck hide window: probe echo arrives only after >=3s of idle (the idle
// release bursts) or never; sentinel/__th7_ text may leak.
// Usage: node scripts/freeze-repro.mjs [runsPerVariant]
import { spawn } from "node:child_process";
import { mkdtempSync, rmSync } from "node:fs";
import { homedir, tmpdir } from "node:os";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const exe = join(root, "backend", "bin", "dbx-plugin-termhive.exe");
const keyPath = join(homedir(), ".ssh", "id_ed25519");
const runsPerVariant = Number(process.argv[2] || 10);

const dataDir = mkdtempSync(join(tmpdir(), "termhive-freeze-"));
const child = spawn(exe, [], {
  stdio: ["pipe", "pipe", "inherit"],
  env: { ...process.env, DBX_PLUGIN_DATA_DIR: dataDir },
});

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
      if (channel.startsWith("term/")) {
        frames.push({ t: Date.now(), channel, data });
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
function sendBinary(channel, data) {
  const channelBytes = Buffer.from(channel, "utf8");
  const payload = Buffer.alloc(2 + channelBytes.length + data.length);
  payload.writeUInt16BE(channelBytes.length, 0);
  channelBytes.copy(payload, 2);
  data.copy(payload, 2 + channelBytes.length);
  child.stdin.write(frame(1, payload));
}
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));

function lifecycle(id) {
  return {
    provider: { id: "com.milii.termhive.provider.ssh", databaseType: "ssh" },
    connection: {
      id,
      db_type: "plugin",
      plugin_id: "com.milii.termhive",
      plugin_connection_provider: "com.milii.termhive.provider.ssh",
      plugin_connection_type: "ssh",
      name: "freeze-repro",
      host: "127.0.0.1",
      port: 22,
      username: process.env.USERNAME || "gordo",
      password: "",
      external_config: { auth_method: "key", key_path: keyPath, encoding: "utf-8" },
      connection_secrets: {},
    },
    runtime: null,
  };
}

// One run mimics the UI's open sequence on one shell. probeDelayMs controls
// when we type (2.5s = handshake surely over; 0.6s = possibly mid-handshake).
async function run(label, openShell, probeDelayMs, keepOpen) {
  frames.length = 0;
  const { shellId, connectionId } = await openShell();
  await sleep(130);
  await call("session/attach", { shellId });
  await sleep(41);
  await call("session/resize", { shellId, cols: 309, rows: 82 });

  await sleep(probeDelayMs);
  const typedAt = Date.now();
  sendBinary(`term/${shellId}`, Buffer.from("echo ZZMARK7\r"));
  await sleep(4600);

  const stream = frames.map((f) => ({ t: f.t, text: f.data.toString("utf8") }));
  const allText = stream.map((f) => f.text).join("");
  const probeFrames = stream.filter((f) => f.text.includes("ZZMARK7"));
  const early = probeFrames.filter((f) => f.t - typedAt < 1600);
  const late = probeFrames.filter((f) => f.t - typedAt >= 1600);
  const prompt = /> |\$P\$G|\x1b\]7;file:\/\//.test(allText);
  const sentinelLeak = allText.includes("__th7_");
  const banner = allText.includes("Microsoft Windows") || allText.length > 0;

  let verdict = "OK";
  if (early.length === 0 && late.length > 0) verdict = "STUCK-LATE-RELEASE";
  else if (probeFrames.length === 0) verdict = "NO-ECHO-AT-ALL";
  else if (sentinelLeak) verdict = "SENTINEL-LEAK";
  else if (!prompt) verdict = "NO-PROMPT";

  const delayNote = probeFrames.length
    ? `firstProbeEchoAt=${Math.min(...probeFrames.map((f) => f.t - typedAt))}ms`
    : "noProbeEcho";
  console.log(
    `${verdict.padEnd(20)} ${label} bytes=${allText.length} banner=${banner} prompt=${prompt} ${delayNote} frames=${frames.length}`,
  );
  if (verdict !== "OK") {
    for (const [index, f] of frames.entries()) {
      const vis = f.data
        .toString("utf8")
        .replace(/\x1b\[[0-9;?]*[a-zA-Z]/g, (m) => `<${m.slice(1)}>`)
        .replace(/\x1b\][^\x07\x1b]*(?:\x07|\x1b\\)/g, "<OSC>")
        .replace(/\r/g, "")
        .slice(0, 200);
      console.log(`    frame[${index}] +${f.t - typedAt}ms ${f.data.length}B | ${vis}`);
    }
  }

  await call("session/close", { shellId });
  if (!keepOpen) {
    await call("connection/disconnect", lifecycle(connectionId));
  }
  return verdict;
}

try {
  await call("plugin/initialize", { host: { protocolVersions: [1] } });

  const tallies = {};
  const tally = (v) => {
    tallies[v] = (tallies[v] || 0) + 1;
  };

  // Variant A: fresh connection every run (disconnect + reopen, like the user)
  for (let index = 0; index < runsPerVariant; index++) {
    const connectionId = `fz-a-${index}`;
    tally(
      await run(`A${index} fresh-conn`, async () => {
        const connected = await call("connection/connect", lifecycle(connectionId));
        return { shellId: connected.shellId, connectionId };
      }, 2500, false),
    );
  }

  // Variant B: one connection, sequential shells (kind cached -> window armed
  // BEFORE any output, the ninth-round timing)
  const sharedId = "fz-b";
  await call("connection/connect", lifecycle(sharedId));
  await sleep(800);
  await call("connection/disconnect", lifecycle(sharedId)); // never mind, reopen to keep flow same
  {
    const connected = await call("connection/connect", lifecycle(sharedId));
    await call("session/close", { shellId: connected.shellId }); // drop the connect-created shell
  }
  for (let index = 0; index < runsPerVariant; index++) {
    tally(
      await run(`B${index} nth-shell`, async () => {
        const created = await call("session/newShell", { connectionId: sharedId, cols: 80, rows: 24 });
        return { shellId: created.shellId, connectionId: sharedId };
      }, 2500, true),
    );
  }
  await call("connection/disconnect", lifecycle(sharedId));

  // Variant C: fresh connection, probe typed early (possibly mid-handshake)
  for (let index = 0; index < Math.ceil(runsPerVariant / 2); index++) {
    const connectionId = `fz-c-${index}`;
    tally(
      await run(`C${index} early-probe`, async () => {
        const connected = await call("connection/connect", lifecycle(connectionId));
        return { shellId: connected.shellId, connectionId };
      }, 600, false),
    );
  }

  console.log("\n==== tallies ====");
  for (const [verdict, count] of Object.entries(tallies)) {
    console.log(`${verdict}: ${count}`);
  }
  child.kill();
  rmSync(dataDir, { recursive: true, force: true });
  process.exit(0);
} catch (error) {
  console.error(`FAILED: ${error.message}`);
  console.error(error.stack);
  child.kill();
  rmSync(dataDir, { recursive: true, force: true });
  process.exit(1);
}
