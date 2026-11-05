// Host-free preview harness: when the UI is opened outside DBX (plain static
// server during development), a mock bridge keeps every view functional —
// a tiny in-memory shell answers terminal input and a fake tree answers fs
// calls. It also injects the dev host's theme tokens so the preview matches
// the unified DBX look. Production always provides the real bridge, so this
// never runs there.
import type { DirEntry } from "./rpc";

type BinaryHandler = (message: { channel: string; data: Uint8Array }) => void;
type EventHandler = (message: { method: string; params?: any }) => void;

// Token sets mirror what the DBX dev host injects into plugin frames.
const themeTokens = {
  dark: {
    "--color-background": "#18181b",
    "--color-foreground": "#f4f4f5",
    "--color-card": "#1b1b1f",
    "--color-card-foreground": "#f4f4f5",
    "--color-muted": "#27272a",
    "--color-muted-foreground": "#a1a1aa",
    "--color-border": "#3f3f46",
    "--color-input": "#3f3f46",
    "--color-ring": "#71717a",
    "--color-primary": "#60a5fa",
    "--color-primary-foreground": "#18181b",
    "--color-destructive": "#f3625f",
    "--color-destructive-foreground": "#18181b",
    "--radius-md": "6px",
    "--radius-lg": "8px",
  },
  light: {
    "--color-background": "#ffffff",
    "--color-foreground": "#27272a",
    "--color-card": "#ffffff",
    "--color-card-foreground": "#27272a",
    "--color-muted": "#f4f4f5",
    "--color-muted-foreground": "#71717a",
    "--color-border": "#e4e4e7",
    "--color-input": "#e4e4e7",
    "--color-ring": "#93c5fd",
    "--color-primary": "#2563eb",
    "--color-primary-foreground": "#ffffff",
    "--color-destructive": "#e7000b",
    "--color-destructive-foreground": "#ffffff",
    "--radius-md": "6px",
    "--radius-lg": "8px",
  },
} as const;

function applyPreviewTheme(): void {
  const appearance = new URLSearchParams(location.search).get("theme") === "light" ? "light" : "dark";
  document.documentElement.dataset.dbxTheme = appearance;
  for (const [name, value] of Object.entries(themeTokens[appearance])) {
    document.documentElement.style.setProperty(name, value);
  }
}

const files = new Map<string, string>([
  ["readme.md", "# server readme\n"],
  ["app.log", "2026-10-26 10:00:01 service started\n".repeat(200)],
  ["backup.tar.gz", "\0".repeat(4_200_000)],
]);
let cwd = "/var/www";

function entries(): DirEntry[] {
  const list: DirEntry[] = [
    { name: "..", size: 0, isDir: true, isSymlink: false, modTime: Date.now() - 86_400_000 },
    { name: "logs", size: 0, isDir: true, isSymlink: false, modTime: Date.now() - 3_600_000 },
    { name: "releases", size: 0, isDir: true, isSymlink: false, modTime: Date.now() - 7200_000 },
  ];
  for (const [name, content] of files) {
    list.push({ name, size: content.length, isDir: false, isSymlink: false, modTime: Date.now() - 600_000 });
  }
  return list;
}

export function installMockBridge(context: Record<string, unknown>): void {
  applyPreviewTheme();
  const binaryHandlers = new Set<BinaryHandler>();
  const eventHandlers = new Set<EventHandler>();
  let history = "";
  let counter = 0;
  const encoder = new TextEncoder();
  const push = (channel: string, data: string | Uint8Array): void => {
    binaryHandlers.forEach((handler) =>
      handler({ channel, data: typeof data === "string" ? encoder.encode(data) : data }),
    );
  };

  const demo = {
    connectionId: "c-demo",
    shellId: "sh-demo",
    kind: "ssh",
    name: "demo-web-01",
    host: "203.0.113.10",
    username: "deploy",
  };
  let shellCounter = 0;

  const mock = {
    ready: Promise.resolve({}),
    context,
    locale: "zh-CN",
    theme: { appearance: document.documentElement.dataset.dbxTheme },
    invoke: async (method: string, params: any = {}) => {
      await new Promise((resolve) => setTimeout(resolve, 60));
      switch (method) {
        case "connection/ensure":
          return { ...demo };
        case "connection/credentials":
          return {
            connectionId: demo.connectionId,
            method: "password",
            password: "demo-pass",
            keyPath: "",
            passphrase: "",
          };
        case "session/newShell":
          shellCounter += 1;
          return { ...demo, shellId: `sh-demo-${shellCounter}` };
        case "connection/list":
          return {
            connections: [
              {
                connectionId: "c-demo-2",
                name: "demo-web-02",
                host: "10.0.0.22",
                port: 22,
                username: "deploy",
              },
            ],
          };
        case "settings/get":
          return { settings: {} };
        case "settings/put":
          return { ok: true };
        case "session/shells":
          return [{ ...demo, closed: false, createdAt: Date.now(), primary: true }];
        case "session/attach":
          return { shellId: params.shellId, closed: false };
        case "session/resize":
        case "session/close":
          return { ok: true };
        case "fs/home":
          return { path: cwd };
        case "fs/list":
          return { path: params.path, entries: entries() };
        case "fs/stat":
          return entries().find((entry) => entry.name === params.path.split("/").pop()) ?? null;
        case "fs/mkdir":
        case "fs/mkdirAll":
        case "fs/remove":
        case "fs/rename":
          return { ok: true };
        case "fs/read":
          return {
            contentBase64: btoa("demo-file-content\nline-2\n"),
            size: 24,
            offset: 0,
            limit: 24,
            windowed: false,
            encoding: "utf-8",
          };
        case "fs/write":
          return { ok: true };
        case "transfer/startUpload":
          return { transferId: "t-demo-" + ++counter };
        case "transfer/startDownload":
          return { transferId: "t-demo-" + ++counter, size: 1024 };
        case "transfer/ready": {
          // preview download demo: once the UI's listeners are registered,
          // pump the promised bytes and finish, so the queue row walks
          // through active → done (and the auto-close fade after it)
          const id = String(params.transferId || "");
          setTimeout(() => {
            for (let i = 0; i < 4; i++) push(`down/${id}`, "A".repeat(256));
            eventHandlers.forEach((handler) =>
              handler({ method: "transfer.done", params: { transferId: id, ok: true } }),
            );
          }, 200);
          return { ok: true };
        }
        case "tail/start":
          return { taskId: "fw-demo" };
        case "tail/ready":
          // a taste of live follow in preview mode: a burst of demo lines,
          // including an ERROR one so the coloring rules show up
          setTimeout(() => {
            push("tail/fw-demo", "[demo] service started, following app.log\r\n");
            let tick = 0;
            const timer = window.setInterval(() => {
              tick += 1;
              push("tail/fw-demo", `2026-11-02 12:00:${String(tick % 60).padStart(2, "0")} INFO  request served in ${tick * 3}ms\r\n`);
              if (tick === 3) push("tail/fw-demo", "2026-11-02 12:00:03 ERROR demo failure line for the coloring rules\r\n");
              if (tick >= 8) window.clearInterval(timer);
            }, 900);
          }, 250);
          return { ok: true };
        case "tail/stop":
          return { ok: true };
        default:
          throw new Error(`mock: ${method} not implemented`);
      }
    },
    request: async (method: string) => {
      if (method === "host.getContext") return context;
      throw new Error(`mock request: ${method}`);
    },
    notify: async (method: string, params: any = {}) => {
      // preview demos ride on the tail/ready and transfer/ready notifications
      if (method === "tail/ready" || method === "transfer/ready") {
        return mock.invoke(method, params);
      }
      return null;
    },
    sendBinary: async (channel: string, data: Uint8Array | string) => {
      const [prefix] = channel.split("/");
      if (prefix === "term") {
        const text = typeof data === "string" ? data : new TextDecoder().decode(data);
        history += text;
        push(channel, text);
        if (text.endsWith("\r")) {
          const command = history.trim();
          history = "";
          const reply = runCommand(command);
          setTimeout(() => push(channel, reply), 120);
        }
      }
    },
    onBinary: (handler: BinaryHandler) => {
      binaryHandlers.add(handler);
      return () => binaryHandlers.delete(handler);
    },
    onEvent: (handler: EventHandler) => {
      eventHandlers.add(handler);
      return () => eventHandlers.delete(handler);
    },
    onContext: () => () => undefined,
    onInit: (handler: EventHandler) => {
      handler(context as any);
      return () => undefined;
    },
    copy: async () => undefined,
    setTitle: async () => undefined,
    closeWorkbench: async () => undefined,
    encodeBase64: (data: Uint8Array) => btoa(String.fromCharCode(...data)),
    decodeBase64: (base64: string) => Uint8Array.from(atob(base64), (c) => c.charCodeAt(0)),
  };

  function runCommand(command: string): string {
    const [name, ...args] = command.trim().split(/\s+/);
    switch (name) {
      case "":
        return "\r\n";
      case "ls":
        return entries().map((entry) => (entry.isDir ? entry.name + "/" : entry.name)).join("    ") + "\r\n";
      case "pwd":
        return cwd + "\r\n";
      case "whoami":
        return "deploy\r\n";
      case "echo":
        return args.join(" ") + "\r\n";
      case "clear":
        return "\x1b[2J\x1b[H";
      case "uname":
        return "Linux demo-web-01 6.1.0 #1 SMP x86_64 GNU/Linux\r\n";
      default:
        return `${name}: command not found\r\n`;
    }
  }

  (window as any).dbxPlugin = mock;

  // greet like a real login
  setTimeout(() => {
    const greeting =
      "Last login: Sun Oct 26 10:02:11 2026 from 192.168.31.124\r\n" +
      "\x1b[1;32mdeploy@demo-web-01\x1b[0m:\x1b[1;34m/var/www\x1b[0m$ ";
    binaryHandlers.forEach((handler) =>
      handler({ channel: "term/sh-demo", data: encoder.encode(greeting) }),
    );
  }, 300);
}
