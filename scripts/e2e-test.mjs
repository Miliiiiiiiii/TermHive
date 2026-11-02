// End-to-end test: spawns the local testsrv SSH/SFTP server plus the sidecar,
// then drives the DBX connection lifecycle, terminal I/O and SFTP
// uploads/downloads over the framed protocol — the same paths DBX will use.
// Usage: node scripts/e2e-test.mjs
import { spawn } from "node:child_process";
import { mkdtempSync, readFileSync, readdirSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { once } from "node:events";

const root = process.cwd();
const sidecarExe = join(root, "backend", "bin", "dbx-plugin-termhive.exe");
const testsrvExe = join(root, "tools", "testsrv", "testsrv.exe");

const server = spawn(testsrvExe, [], { stdio: ["ignore", "pipe", "inherit"] });
const readyLine = (await once(server.stdout, "data")).toString();
const [, port, sftpRoot] = readyLine.trim().split(/\s+/);
console.log(`testsrv on :${port}, sftp root ${sftpRoot}`);

// The data dir survives sidecar restarts — that is the whole point of the
// credential cache. Run both sidecar generations against the same dir.
const dataDir = mkdtempSync(join(tmpdir(), "termhive-e2e-"));

function spawnSidecar() {
  return spawn(sidecarExe, [], {
    stdio: ["pipe", "pipe", "inherit"],
    env: { ...process.env, DBX_PLUGIN_DATA_DIR: dataDir },
  });
}

let child = spawnSidecar();
const pending = new Map();
const notifications = [];
const binaryByChannel = new Map();
let seq = 0;
let buffer = Buffer.alloc(0);

function attachReader(sidecar) {
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
        } else if (message.method) {
          notifications.push(message);
        }
      } else if (kind === 1) {
        const channelLength = payload.readUInt16BE(0);
        const channel = payload.subarray(2, 2 + channelLength).toString("utf8");
        const data = payload.subarray(2 + channelLength);
        if (!binaryByChannel.has(channel)) binaryByChannel.set(channel, []);
        binaryByChannel.get(channel).push(data);
      }
    }
  });
}

attachReader(child);

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
function expect(condition, label) {
  if (!condition) throw new Error(`assertion failed: ${label}`);
  console.log(`  ok ${label}`);
}
function channelData(channel) {
  return Buffer.concat(binaryByChannel.get(channel) ?? []);
}
function waitFor(predicate, label, timeoutMs = 20000) {
  return new Promise((resolve, reject) => {
    const started = Date.now();
    const timer = setInterval(() => {
      const found = notifications.find(predicate);
      if (found) {
        clearInterval(timer);
        resolve(found);
      } else if (Date.now() - started > timeoutMs) {
        clearInterval(timer);
        reject(new Error(`timeout waiting for ${label}`));
      }
    }, 25);
  });
}
function waitChannelIncludes(channel, needle, label, timeoutMs = 10000) {
  return new Promise((resolve, reject) => {
    const started = Date.now();
    const timer = setInterval(() => {
      if (channelData(channel).toString("utf8").includes(needle)) {
        clearInterval(timer);
        console.log(`  ok ${label}`);
        resolve(true);
      } else if (Date.now() - started > timeoutMs) {
        clearInterval(timer);
        reject(new Error(`timeout waiting for ${label}`));
      }
    }, 50);
  });
}

// lifecycle payload in the exact shape the host sends (see dev-host
// connections.mjs lifecyclePayload()).
function lifecycle(connectionId = "conn-e2e-1") {
  return {
    provider: { id: "com.milii.termhive.provider.ssh", databaseType: "ssh" },
    connection: {
      id: connectionId,
      db_type: "plugin",
      plugin_id: "com.milii.termhive",
      plugin_connection_provider: "com.milii.termhive.provider.ssh",
      plugin_connection_type: "ssh",
      name: "e2e-host",
      host: "127.0.0.1",
      port: Number(port),
      username: "tester",
      password: "pw123",
      external_config: { auth_method: "password", encoding: "utf-8", keepalive_secs: 30 },
      connection_secrets: {},
      read_only: false,
      query_timeout_secs: 60,
      idle_timeout_secs: 60,
    },
    runtime: null,
  };
}

try {
  await call("plugin/initialize", { host: { protocolVersions: [1] } });
  await call("settings/put", { settings: { probe: "e2e", scheme: "oneDark" } });
  await waitFor((n) => n.method === "settings.changed" && n.params?.settings?.probe === "e2e", "settings.changed broadcast");

  // ---- connection lifecycle: test / connect / ensure ----
  const test = await call("connection/test", lifecycle());
  expect(test.success === true, "connection/test succeeds against live server");

  const badTest = await call("connection/test", {
    ...lifecycle("conn-e2e-bad"),
    connection: { ...lifecycle().connection, id: "conn-e2e-bad", password: "wrong-password" },
  }).then(
    () => {
      throw new Error("bad test should fail");
    },
    (error) => error.message,
  );
  expect(/auth|dial|connect/i.test(badTest), `wrong credentials rejected: ${badTest.slice(0, 50)}`);

  const connected = await call("connection/connect", lifecycle());
  expect(connected.success === true && connected.shellId, `connection/connect returns shell: ${connected.shellId}`);
  const shellId = connected.shellId;
  expect(connected.connectionId === "conn-e2e-1" && connected.kind === "ssh", "connect info carries connection metadata");

  const again = await call("connection/connect", lifecycle());
  expect(again.shellId === shellId, "connect is idempotent per connection id");

  // ---- host re-issued a fresh record id for the same saved connection ----
  const reissued = await call("connection/ensure", { connectionId: "conn-e2e-reissued" });
  expect(
    reissued.shellId === shellId && reissued.host === "127.0.0.1",
    "ensure with a reissued connection id still resolves the single cached connection",
  );

  const ensured = await call("connection/ensure", { connectionId: "conn-e2e-1" });
  expect(ensured.shellId === shellId, "connection/ensure returns the live primary shell");

  // ---- plaintext credentials retrievable while connected ----
  const credentials = await call("connection/credentials", { connectionId: "conn-e2e-1" });
  expect(credentials.password === "pw123", "connection/credentials returns the plaintext password");

  await new Promise((resolve) => setTimeout(resolve, 300));
  expect(channelData(`term/${shellId}`).toString("utf8").includes("TEST-BANNER"), "terminal banner received");

  // testsrv never EXECUTES commands, so the injection's echo output (the
  // second sentinel occurrence) never comes and the hide window self-closes
  // on the idle timeout; real shells close it within milliseconds
  await new Promise((resolve) => setTimeout(resolve, 3600));
  sendBinary(`term/${shellId}`, Buffer.from("hello e2e\n", "utf8"));
  await new Promise((resolve) => setTimeout(resolve, 300));
  expect(channelData(`term/${shellId}`).toString("utf8").includes("ECHO:hello e2e"), "terminal input echoed back");

  const home = await call("fs/home", { shellId });
  expect(typeof home.path === "string" && home.path.length > 0, `sftp home: ${home.path}`);

  // ---- navigation paths come back normalized (no ".." accumulation) ----
  const mangled = await call("fs/list", { shellId, path: `${home.path}/../../` });
  expect(!mangled.path.includes(".."), `fs/list normalizes paths: ${mangled.path}`);

  // ---- terminal follow: hook injected, its echo hidden from the stream ----
  // The injection line is wrapped in a sentinel-suppressed output window: the
  // terminal (and the ring buffer, and ConPTY's resize replay) must never see
  // the hook bytes. The keystroke echo above proves the window closed and the
  // channel flows.
  expect(
    !channelData(`term/${shellId}`).toString("utf8").includes("__osc7"),
    "follow hook echo is suppressed from the terminal stream",
  );

  // ---- uploading onto a directory must fail with an explicit message ----
  const dirUpload = await call("transfer/startUpload", { shellId, remotePath: home.path, size: 4 }).then(
    () => null,
    (error) => error.message,
  );
  expect(/文件夹/.test(dirUpload ?? ""), `directory upload rejected with a clear reason: ${(dirUpload ?? "").slice(0, 40)}`);

  // ---- upload over the up/<transferId> channel ----
  const payload = Buffer.from("HELLO-SFTP".repeat(100000)); // ~1.1 MB, multiple chunks
  const upload = await call("transfer/startUpload", { shellId, remotePath: join(home.path, "up.bin"), size: payload.length });
  const upId = upload.transferId;
  let cursor = 0;
  while (cursor < payload.length) {
    const chunk = payload.subarray(cursor, cursor + 256 * 1024);
    sendBinary(`up/${upId}`, chunk);
    await waitFor((n) => n.method === "transfer.ack" && n.params.transferId === upId && n.params.received >= cursor + chunk.length, `upload ack @${cursor + chunk.length}`);
    cursor += chunk.length;
  }
  const upDone = await waitFor((n) => n.method === "transfer.done" && n.params.transferId === upId, "upload done");
  expect(upDone.params.ok === true && upDone.params.bytes === payload.length, `upload completed (${payload.length} bytes)`);

  const listed = await call("fs/list", { shellId, path: home.path });
  const entry = listed.entries.find((item) => item.name === "up.bin");
  expect(Boolean(entry) && entry.size === payload.length, "uploaded file listed with size");

  // ---- in-app editor: fs/write creates/overwrites, fs/read roundtrips ----
  const notePath = join(home.path, "note.txt");
  await call("fs/write", { shellId, path: notePath, contentBase64: Buffer.from("你好 termhive\nline-2\n").toString("base64") });
  const read1 = await call("fs/read", { shellId, path: notePath });
  expect(
    Buffer.from(read1.contentBase64, "base64").toString("utf8") === "你好 termhive\nline-2\n",
    "fs/read returns written UTF-8 content",
  );
  expect(read1.binary !== true && read1.windowed === false, "fs/read flags text content correctly");
  const binPath = join(home.path, "blob.bin");
  await call("fs/write", { shellId, path: binPath, contentBase64: Buffer.from("AB\0CD\0\0\x01\x02").toString("base64") });
  const readBin = await call("fs/read", { shellId, path: binPath });
  expect(readBin.binary === true, "fs/read reports NUL-containing files as binary");
  // ---- windowed reads: files above 2 MiB load one window at a time ----
  const bigPayload = Buffer.alloc(2 * 1024 * 1024 + 1024);
  for (let index = 0; index < bigPayload.length; index += 1) bigPayload[index] = 65 + (index % 26);
  const bigPath = join(home.path, "big.log");
  await call("fs/write", { shellId, path: bigPath, contentBase64: bigPayload.toString("base64") });
  const win1 = await call("fs/read", { shellId, path: bigPath, offset: 0 });
  expect(
    win1.windowed === true && win1.limit === 2 * 1024 * 1024,
    `fs/read windows large files (${win1.limit} bytes)`,
  );
  expect(Buffer.from(win1.contentBase64, "base64").equals(bigPayload.subarray(0, win1.limit)), "first window bytes match");
  const tailOff = bigPayload.length - 1024;
  const winTail = await call("fs/read", { shellId, path: bigPath, offset: tailOff });
  expect(
    winTail.windowed === true && winTail.offset === tailOff,
    "tail window still flags the file as windowed (read-only mode)",
  );
  expect(Buffer.from(winTail.contentBase64, "base64").equals(bigPayload.subarray(tailOff)), "tail window bytes match");
  await call("fs/write", { shellId, path: notePath, contentBase64: Buffer.from("edited 你好\n").toString("base64") });
  const read2 = await call("fs/read", { shellId, path: notePath });
  expect(
    Buffer.from(read2.contentBase64, "base64").toString("utf8") === "edited 你好\n",
    "fs/write overwrites an existing file",
  );
  const tempGone = await call("fs/read", { shellId, path: notePath + ".termhive.tmp" }).then(
    () => false,
    () => true,
  );
  expect(tempGone === true, "editor temp file is renamed away after save");

  // ---- MCP tool surface: discovery, gating and execution ----
  const catalog = await call("mcp/tools", { connectionId: "conn-e2e-1" });
  const names = catalog.tools.map((tool) => tool.name).sort();
  expect(names.length === 12, `mcp/tools lists 12 tools (got ${names.length})`);
  expect(
    names.join(",") ===
      "exec,exec_readonly,sftp_list_dir,sftp_mkdir,sftp_read_file,sftp_remove,sftp_rename,sftp_stat,sftp_write_file,tail_read,tail_start,tail_stop",
    "mcp/tools catalog names match",
  );
  expect(
    catalog.tools.filter((tool) => tool.annotations?.readOnlyHint === true).length === 7,
    "7 tools are annotated read-only",
  );
  expect(
    catalog.tools.every((tool) => tool.inputSchema?.properties?.dbx_connection && tool.inputSchema?.properties?.connectionId),
    "every tool schema carries the connection binding properties",
  );
  const unknownTool = await call("mcp/call", { tool: "nope", arguments: {} }).then(
    () => false,
    () => true,
  );
  expect(unknownTool === true, "mcp/call rejects an unknown tool as a protocol error");

  const ro = await call("mcp/call", {
    tool: "exec_readonly",
    arguments: { command: "echo hello", connectionId: "conn-e2e-1" },
  });
  expect(ro.isError !== true && ro.content[0].text.includes("EXEC:echo hello"), "exec_readonly runs on the exec channel");

  const roDenied = await call("mcp/call", {
    tool: "exec_readonly",
    arguments: { command: "env", connectionId: "conn-e2e-1" },
  });
  expect(
    roDenied.isError === true && roDenied.content[0].text.includes("not a read-only command"),
    "exec_readonly refuses non-allowlisted programs",
  );

  const bomb = await call("mcp/call", {
    tool: "exec",
    arguments: { command: "mkfs.ext4 /dev/sdb1", connectionId: "conn-e2e-1" },
  });
  expect(
    bomb.isError === true && bomb.content[0].text.includes("safety gate"),
    "exec refuses catastrophic patterns even though it is approval-gated",
  );

  const noConn = await call("mcp/call", { tool: "sftp_list_dir", arguments: {} });
  // exactly one connection is open here, so resolution succeeds implicitly…
  expect(noConn.isError !== true, "single open connection resolves without dbx_connection");
  expect(noConn.content[0].text.includes("up.bin"), "sftp_list_dir returns JSON entries over the resolved connection");

  const readSlice = await call("mcp/call", {
    tool: "sftp_read_file",
    arguments: { path: notePath, connectionId: "conn-e2e-1" },
  });
  expect(
    readSlice.isError !== true && readSlice.content[0].text.includes("edited 你好"),
    "sftp_read_file returns transcoded content",
  );

  const rootRm = await call("mcp/call", {
    tool: "sftp_remove",
    arguments: { path: "/", recursive: true, connectionId: "conn-e2e-1" },
  });
  expect(
    rootRm.isError === true && rootRm.content[0].text.includes("refusing"),
    "sftp_remove refuses the filesystem root",
  );

  // ---- MCP follow tools: fixed-shape commands, honest failure reporting ----
  const tailNoArgs = await call("mcp/call", { tool: "tail_start", arguments: { connectionId: "conn-e2e-1" } });
  expect(
    tailNoArgs.isError === true && tailNoArgs.content[0].text.includes("path"),
    "tail_start demands a path or unit",
  );
  // testsrv answers every exec by printing EXEC:<command> and exiting at
  // once, so a follow stream "ends immediately" — the start must fail with
  // the captured output instead of handing out a dead taskId
  const tailImmediate = await call("mcp/call", {
    tool: "tail_start",
    arguments: { path: join(home.path, "up.bin"), connectionId: "conn-e2e-1" },
  });
  expect(
    tailImmediate.isError === true && tailImmediate.content[0].text.includes("ended immediately"),
    "tail_start reports an immediately-ending stream as a failed start",
  );
  expect(
    tailImmediate.content[0].text.includes("EXEC:tail"),
    "the failed follow start carries the program's output",
  );
  const tailUnknown = await call("mcp/call", {
    tool: "tail_read",
    arguments: { taskId: "tf-none", connectionId: "conn-e2e-1" },
  });
  expect(
    tailUnknown.isError === true && tailUnknown.content[0].text.includes("no follow task"),
    "tail_read refuses unknown task ids",
  );

  // ---- download over down/<transferId>, acking each chunk ----
  const download = await call("transfer/startDownload", { shellId, remotePath: join(home.path, "up.bin") });
  const downId = download.transferId;
  // gated-pump regression: no bytes may flow before transfer/ready — in the
  // real host the save dialog runs before the UI subscribes, and its demux
  // drops unsubscribed frames (the 0-byte download bug)
  await new Promise((resolve) => setTimeout(resolve, 300));
  expect(channelData(`down/${downId}`).length === 0, "no download bytes flow before transfer/ready");
  await call("transfer/ready", { transferId: downId });
  const downDone = await waitFor(
    (n) => {
      if (n.method === "transfer.done" && n.params.transferId === downId && n.params.ok) return true;
      if (n.method === "transfer.progress" && n.params.transferId === downId) call("transfer/ackDown", { transferId: downId }).catch(() => {});
      return false;
    },
    "download done",
    60000,
  );
  expect(downDone.params.bytes === payload.length, "download reported full size");
  expect(channelData(`down/${downId}`).equals(payload), "downloaded bytes match upload payload");

  // ---- multi-select batch download zips into one stream ----
  const zipStart = await call("transfer/startDownloadZip", {
    shellId,
    paths: [join(home.path, "up.bin"), home.path],
  });
  const zipId = zipStart.transferId;
  await call("transfer/ready", { transferId: zipId });
  await waitFor(
    (n) => {
      if (n.method === "transfer.done" && n.params.transferId === zipId && n.params.ok) return true;
      if (n.method === "transfer.progress" && n.params.transferId === zipId) call("transfer/ackDown", { transferId: zipId }).catch(() => {});
      return false;
    },
    "zip download done",
    60000,
  );
  const zipBytes = channelData(`down/${zipId}`);
  expect(zipBytes.length >= 4 && zipBytes[0] === 0x50 && zipBytes[1] === 0x4b, "zip download starts with PK header");
  // 1 MiB of repeated payload compresses to a few KiB, so assert on entries
  expect(zipBytes.toString("latin1").includes("up.bin"), "zip contains the up.bin entry");

  // ---- log follow mode: tail/<taskId> streams the file's growth ----
  const logPath = join(home.path, "follow.log");
  await call("fs/write", { shellId, path: logPath, contentBase64: Buffer.from("line-1\nline-2\n").toString("base64") });
  const followStart = await call("tail/start", { shellId, path: logPath, lines: 10 });
  const followId = followStart.taskId;
  expect(Boolean(followId), "tail/start registers a follower");
  // gated like downloads: nothing may flow before tail/ready (the UI demux
  // drops unsubscribed frames)
  await new Promise((resolve) => setTimeout(resolve, 300));
  expect(channelData(`tail/${followId}`).length === 0, "no follow bytes flow before tail/ready");
  await call("tail/ready", { taskId: followId });
  await waitChannelIncludes(`tail/${followId}`, "line-2", "initial tail window streams");
  // growth: fs/write replaces the file with longer content; the follower
  // (re-opening by path each poll) picks up the appended bytes
  await call("fs/write", {
    shellId,
    path: logPath,
    contentBase64: Buffer.from("line-1\nline-2\nline-3-appended\n").toString("base64"),
  });
  await waitChannelIncludes(`tail/${followId}`, "line-3-appended", "followed growth streams");
  await call("tail/stop", { taskId: followId });
  await waitFor(
    (n) => n.method === "tail.done" && n.params.taskId === followId,
    "follow teardown notification",
  );

  // ---- second session to the same server (multi-session requirement) ----
  const second = await call("session/newShell", { connectionId: "conn-e2e-1", cols: 80, rows: 24 });
  expect(Boolean(second.shellId) && second.shellId !== shellId, "newShell opens an independent second shell");
  await new Promise((resolve) => setTimeout(resolve, 200));
  expect(channelData(`term/${second.shellId}`).toString("utf8").includes("TEST-BANNER"), "second shell has its own PTY");
  // same testsrv caveat as above: its injection hide window closes on the
  // idle timeout, so later keystroke-echo assertions must wait it out
  await new Promise((resolve) => setTimeout(resolve, 3600));
  const shells = await call("session/shells");
  const live = shells.filter((item) => !item.closed);
  expect(live.length === 2, "two live shells registered");
  expect(live.every((item) => item.connectionId === "conn-e2e-1"), "shells carry the connection id");

  // ---- closing the first session must not disturb the second ----
  // (the multiplexed-transport regression: sessions share one SSH client)
  await call("session/close", { shellId: shellId });
  await new Promise((resolve) => setTimeout(resolve, 300));
  const survivor = await call("session/attach", { shellId: second.shellId });
  expect(survivor.closed === false, "closing session 1 leaves session 2 attached and alive");
  sendBinary(`term/${second.shellId}`, Buffer.from("still-alive\n", "utf8"));
  await new Promise((resolve) => setTimeout(resolve, 300));
  expect(
    channelData(`term/${second.shellId}`).toString("utf8").includes("ECHO:still-alive"),
    "session 2 still echoes input after session 1 closed",
  );
  const afterClose = await call("session/shells");
  expect(afterClose.filter((item) => !item.closed).length === 1, "exactly one shell remains after closing the first");

  // ---- attach replays recent output (on the surviving session 2) ----
  binaryByChannel.delete(`term/${second.shellId}`);
  await call("session/attach", { shellId: second.shellId });
  expect(
    channelData(`term/${second.shellId}`).toString("utf8").includes("ECHO:still-alive"),
    "attach replays ring buffer",
  );

  // ---- session logging: toggle on via merged settings, output lands on disk
  // with per-line timestamps in a per-connection directory ----
  await call("settings/put", { settings: { sessionLog: true } });
  const merged = await call("settings/get");
  expect(merged.settings?.scheme === "oneDark", "settings/put merges instead of replacing");
  sendBinary(`term/${second.shellId}`, Buffer.from("log-me-please\n", "utf8"));
  await new Promise((resolve) => setTimeout(resolve, 800));
  const logDir = join(dataDir, "logs");
  const connDirs = readdirSync(logDir);
  expect(connDirs.length === 1 && connDirs[0] === "tester@127.0.0.1_" + port, "session logs land in a per-connection directory");
  const logFiles = readdirSync(join(logDir, connDirs[0]));
  const sessionLog = logFiles
    .map((name) => readFileSync(join(logDir, connDirs[0], name), "utf8"))
    .find((text) => text.includes("log-me-please"));
  expect(Boolean(sessionLog), "session log captures terminal output");
  expect(/^\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\.\d{3}\] /m.test(sessionLog), "session log lines carry timestamps");
  await call("settings/put", { settings: { sessionLog: false } });

  // ---- sidecar restart: the host may never resend the lifecycle, so the
  // credential cache on disk must let "reconnect" redial on its own ----
  child.kill();
  await new Promise((resolve) => setTimeout(resolve, 400));
  child = spawnSidecar();
  buffer = Buffer.alloc(0);
  attachReader(child);
  await call("plugin/initialize", { host: { protocolVersions: [1] } });
  const revived = await call("connection/ensure", { connectionId: "conn-e2e-1" });
  expect(Boolean(revived.shellId) && revived.shellId !== shellId, "ensure redials from the persisted spec after sidecar restart");
  await new Promise((resolve) => setTimeout(resolve, 300));
  expect(channelData(`term/${revived.shellId}`).toString("utf8").includes("TEST-BANNER"), "revived shell has a fresh PTY");
  const revivedShells = await call("session/shells");
  expect(revivedShells.filter((item) => !item.closed).length === 1, "revived shell is the only live one");

  // ---- UI preferences persist across sidecar restarts ----
  const saved = await call("settings/get");
  expect(saved.settings?.probe === "e2e" && saved.settings?.scheme === "oneDark", "settings survive sidecar restart");

  // ---- cross-server sessions: a second saved connection + its shell in the
  // same workbench (split view across servers rides on this) ----
  const list1 = await call("connection/list");
  expect(
    Array.isArray(list1.connections) && list1.connections.some((item) => item.connectionId === "conn-e2e-1"),
    "connection/list includes the cached connection",
  );
  expect(
    list1.connections.every((item) => !("password" in item) && !("auth" in item)),
    "connection/list never leaks credentials",
  );
  // a genuinely different endpoint: same testsrv process, but a different
  // host string — two saved servers. (With an identical host string the
  // connect fallback deliberately absorbs it as a reissued record id.)
  const secondLifecycle = () => ({
    ...lifecycle("conn-e2e-2"),
    connection: { ...lifecycle("conn-e2e-2").connection, host: "localhost", name: "e2e-host-2" },
  });
  await call("connection/connect", secondLifecycle());
  const crossShell = await call("session/newShell", { connectionId: "conn-e2e-2", cols: 80, rows: 24 });
  expect(Boolean(crossShell.shellId), "newShell opens a session of the second connection");
  const list2 = await call("connection/list");
  expect(list2.connections.length >= 2, "connection/list sees both cached connections");
  await new Promise((resolve) => setTimeout(resolve, 250));
  expect(
    channelData(`term/${crossShell.shellId}`).toString("utf8").includes("TEST-BANNER"),
    "cross-server session streams its own terminal output",
  );
  await call("connection/disconnect", lifecycle("conn-e2e-2"));

  // ---- secrets at rest: specs sealed, server fingerprints recorded ----
  const specsRaw = readFileSync(join(dataDir, "specs.json"), "utf8");
  expect(!specsRaw.includes("pw123"), "specs.json does not contain the plaintext password");
  const specsBox = JSON.parse(specsRaw);
  expect(specsBox.scheme === "dpapi" && specsBox.data.length > 0, `specs.json sealed (${specsBox.scheme})`);
  const knownRaw = readFileSync(join(dataDir, "known_hosts.json"), "utf8");
  expect(knownRaw.includes("SHA256:"), "known_hosts.json recorded the testsrv fingerprint");
  const hostList = await call("hostkey/list");
  expect(
    Array.isArray(hostList.hosts) && hostList.hosts.length >= 1,
    `hostkey/list reports ${hostList.hosts.length} record(s)`,
  );
  expect(String(hostList.hosts[0].fingerprint).startsWith("SHA256:"), "fingerprint is SHA256 format");

  // ---- auto-reconnect: killing the server must wake the supervisor ----
  server.kill();
  const reconnecting = await waitFor(
    (n) => n.method === "connection.reconnecting" && n.params.connectionId === "conn-e2e-1",
    "connection.reconnecting after server death",
    20000,
  );
  expect(reconnecting.params.attempt >= 1, "reconnecting reports its attempt number");
  expect(typeof reconnecting.params.nextInMs === "number" && reconnecting.params.nextInMs > 0, "reconnecting reports the backoff delay");

  // ---- disconnect lifecycle tears everything down (also cancels the
  // supervisor retrying the dead server) ----
  await call("connection/disconnect", lifecycle());
  await new Promise((resolve) => setTimeout(resolve, 300));
  const afterDisconnect = await call("session/shells");
  expect(afterDisconnect.filter((item) => !item.closed).length === 0, "connection/disconnect closes all shells");

  const ensureGone = await call("connection/ensure", { connectionId: "conn-e2e-1" }).then(
    () => {
      throw new Error("ensure after disconnect should fail");
    },
    (error) => error.message,
  );
  expect(/尚未建立/.test(ensureGone), "ensure after disconnect tells the user to reopen from the connection list");

  const credentialsGone = await call("connection/credentials", { connectionId: "conn-e2e-1" }).then(
    () => {
      throw new Error("credentials after disconnect should fail");
    },
    (error) => error.message,
  );
  expect(/尚未建立/.test(credentialsGone), "plaintext credentials are gone after disconnect");

  // fingerprints survive disconnect by design; Remove is the reset path
  const removedKey = await call("hostkey/remove", { host: "127.0.0.1", port: Number(port) });
  expect(removedKey.removed === true, "hostkey/remove drops the testsrv fingerprint record");

  console.log("\ne2e test passed");
  child.kill();
  server.kill();
  rmSync(dataDir, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 });
  process.exit(0);
} catch (error) {
  console.error(`\nFAILED: ${error.message}`);
  child.kill();
  server.kill();
  rmSync(dataDir, { recursive: true, force: true, maxRetries: 10, retryDelay: 300 });
  process.exit(1);
}
