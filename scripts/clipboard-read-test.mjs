// Unit test for the scripted clipboard read behind the terminal's menu paste
// (ui/src/host.ts readClipboard), run against a fake host bridge — no browser,
// no DBX. Also guards the manifest coupling that read depends on: the host
// refuses `clipboard.readText` unless the plugin declares
// `host.clipboard:read`, and DBX builds older than 0.6.23 reject the manifest
// for declaring it, so the engines floor must not sit below that.
// Usage: node --experimental-strip-types scripts/clipboard-read-test.mjs
import { readFileSync } from "node:fs";
import { join, dirname } from "node:path";
import { fileURLToPath } from "node:url";

const root = join(dirname(fileURLToPath(import.meta.url)), "..");
const { readClipboard } = await import("../ui/src/host.ts");

let failed = 0;
const check = (name, ok) => {
  console.log(`${ok ? "ok" : "FAIL"}  ${name}`);
  if (!ok) failed += 1;
};

// host.ts reads `window.dbxPlugin`, `window.setTimeout` and
// `navigator.clipboard` at call time; swap them per case. The retry timer
// fires at once so the rate-limit cases don't sleep for real.
function install({ plugin, readText, setTimeoutFn = (fn) => fn() }) {
  globalThis.window = { dbxPlugin: plugin, setTimeout: setTimeoutFn };
  Object.defineProperty(globalThis, "navigator", {
    value: { clipboard: readText ? { readText } : undefined },
    configurable: true,
    writable: true,
  });
}

const hostBridge = (readText, clipboardRead = true) => ({
  capabilities: { clipboardRead, clipboardWrite: true },
  clipboard: { readText },
});
const denyNavigator = async () => {
  throw new DOMException("Read permission denied.", "NotAllowedError");
};

const cases = [
  [
    "host bridge returns the clipboard text",
    { plugin: hostBridge(async () => "ls -la"), readText: denyNavigator },
    { ok: true, text: "ls -la" },
  ],
  [
    "an empty clipboard reads as empty text, not as a failure",
    { plugin: hostBridge(async () => ""), readText: denyNavigator },
    { ok: true, text: "" },
  ],
  [
    "declined consent is reported as denied (it is remembered for the session)",
    {
      plugin: hostBridge(async () => {
        throw new Error("Clipboard read was denied for this plugin session");
      }),
      readText: async () => "should not be consulted",
    },
    { ok: false, reason: "denied" },
  ],
  [
    "a rate-limited read waits out the window and retries",
    {
      plugin: hostBridge((() => {
        let calls = 0;
        return async () => {
          calls += 1;
          if (calls === 1) throw new Error("Clipboard read rate limit exceeded; retry in a moment");
          return "second try";
        };
      })()),
      readText: denyNavigator,
    },
    { ok: true, text: "second try" },
  ],
  [
    "a read still rate-limited after the retry is reported as busy",
    {
      plugin: hostBridge(async () => {
        throw new Error("Clipboard read rate limit exceeded; retry in a moment");
      }),
      readText: async () => "should not be consulted",
    },
    { ok: false, reason: "busy" },
  ],
  [
    "any other host error is a plain failure",
    {
      plugin: hostBridge(async () => {
        throw new Error("Host clipboard read returned a non-string value");
      }),
      readText: async () => "should not be consulted",
    },
    { ok: false, reason: "failed" },
  ],
  [
    "a host without the clipboardRead capability is never asked",
    {
      plugin: hostBridge(async () => {
        throw new Error("must not be called");
      }, false),
      readText: async () => "from navigator",
    },
    { ok: true, text: "from navigator" },
  ],
  [
    "a host that omits capabilities entirely (older host) falls back to navigator",
    {
      plugin: { clipboard: { readText: async () => "unused" } },
      readText: async () => "from navigator",
    },
    { ok: true, text: "from navigator" },
  ],
  [
    "no readable clipboard anywhere is reported as unsupported",
    { plugin: { clipboard: { readText: async () => "unused" } }, readText: denyNavigator },
    { ok: false, reason: "unsupported" },
  ],
  [
    "no bridge object at all is reported as unsupported",
    { plugin: undefined, readText: undefined },
    { ok: false, reason: "unsupported" },
  ],
];

for (const [name, env, expected] of cases) {
  install(env);
  let actual;
  try {
    actual = await readClipboard();
  } catch (error) {
    actual = { threw: error.message };
  }
  check(name, JSON.stringify(actual) === JSON.stringify(expected));
}

// ---- manifest coupling ----------------------------------------------------

const manifest = JSON.parse(readFileSync(join(root, "manifest.json"), "utf8"));
check(
  "manifest declares host.clipboard:read (the host rejects clipboardRead without it)",
  Array.isArray(manifest.permissions) && manifest.permissions.includes("host.clipboard:read"),
);
// DBX v0.6.23 is the first build that knows the permission; anything older
// fails the install with "Unsupported plugin Host API permission".
const floor = /^>=(\d+)\.(\d+)\.(\d+)$/.exec(String(manifest.engines?.dbx ?? ""));
const floorValue = floor ? floor.slice(1).map(Number) : null;
const firstSupported = [0, 6, 23];
const floorIsHighEnough =
  floorValue !== null &&
  floorValue.reduce((cmp, part, i) => cmp || part - firstSupported[i], 0) >= 0;
check(
  `manifest engines.dbx (${manifest.engines?.dbx}) is not below 0.6.23`,
  floorIsHighEnough,
);

if (failed > 0) {
  console.error(`\nclipboard-read test: ${failed} check(s) failed`);
  process.exit(1);
}
console.log("\nclipboard-read test passed");
