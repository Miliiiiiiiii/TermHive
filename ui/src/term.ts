// xterm.js wrapper: themed terminal with fit handling, GPU rendering,
// clickable links and a self-contained search bar (Ctrl+F).
import { Terminal } from "@xterm/xterm";
import type { ITheme } from "@xterm/xterm";
import { FitAddon } from "@xterm/addon-fit";
import { SearchAddon } from "@xterm/addon-search";
import { WebLinksAddon } from "@xterm/addon-web-links";
import { WebglAddon } from "@xterm/addon-webgl";
import { findScheme } from "./theme";
import "@xterm/xterm/css/xterm.css";

export function isLightTheme(): boolean {
  // the real host reports its appearance through the bridge; data-dbxTheme
  // only exists under the mock dev host, and prefers-color-scheme follows
  // the OS, not the DBX app theme (a light DBX theme on a dark-OS machine
  // was misdetected as dark)
  const appearance = window.dbxPlugin?.theme?.appearance;
  if (appearance === "light") return true;
  if (appearance === "dark") return false;
  const data = document.documentElement.dataset.dbxTheme;
  if (data === "light") return true;
  if (data === "dark") return false;
  return window.matchMedia?.("(prefers-color-scheme: light)").matches ?? false;
}

export interface TerminalOptions {
  // Fired when the shell reports its working directory via OSC 7 / OSC 9;9
  // file URLs (shell integration hooks injected by the sidecar). Drives the
  // file panel's follow mode.
  onCwd?: (path: string) => void;
  // font/scrollback preferences from the plugin settings (defaults apply
  // when absent — the mini window and log viewer ride the same settings)
  style?: TerminalStyle;
}

export interface TerminalStyle {
  /** "" = follow the host's --font-mono / Cascadia fallback chain */
  fontFamily: string;
  fontSize: number;
  scrollback: number;
}

export function defaultTerminalStyle(): TerminalStyle {
  return { fontFamily: "", fontSize: 13, scrollback: 5000 };
}

export interface TerminalHandle {
  terminal: Terminal;
  fit: () => void;
  write: (data: Uint8Array | string) => void;
  onData: (handler: (data: string) => void) => void;
  reset: () => void;
  dispose: () => void;
  cols: () => number;
  rows: () => number;
  openSearch: () => void;
  setScheme: (schemeId: string) => void;
  applyStyle: (style: TerminalStyle) => void;
}

// oscToPath extracts an SFTP-style path from an OSC file-URL payload
// ("file://host/C:/x" or "file:///home/u", optionally quoted or percent
// encoded; cmd.exe hooks send backslashes). Anything that does not carry a
// file URL with a path is ignored (null).
function oscToPath(data: string): string | null {
  const marker = "file://";
  const at = data.indexOf(marker);
  if (at === -1) return null;
  let rest = data
    .slice(at + marker.length)
    .replace(/^["']|["']$/g, "")
    .trim();
  const slash = rest.indexOf("/");
  if (slash === -1) return null; // host only, no path component
  rest = rest.slice(slash);
  let path = rest;
  try {
    path = decodeURIComponent(rest);
  } catch {
    // literal % in the path: keep the raw form
  }
  // drive-letter payloads (cmd PROMPT hook) arrive with backslashes
  if (/^\/[A-Za-z]:[\\/]/.test(path)) path = path.replace(/\\/g, "/");
  if (!path.startsWith("/") || path.length <= 1) return null;
  return path;
}

// Terminal colors follow the host theme tokens, falling back to the zinc
// palette the dev host injects.
function tokenColor(name: string, fallback: string): string {
  return getComputedStyle(document.documentElement).getPropertyValue(name).trim() || fallback;
}

function isChinese(): boolean {
  return navigator.language?.toLowerCase().startsWith("zh");
}

// The search bar ships inside term.ts so every terminal gets Ctrl+F without
// each view wiring one; styling is inline to stay independent of style.css.
function buildSearchBar(container: HTMLElement, search: SearchAddon, onClose: () => void): HTMLElement {
  const zh = isChinese();
  const bar = document.createElement("div");
  bar.style.cssText =
    "position:absolute;top:6px;right:12px;z-index:30;display:none;align-items:center;gap:4px;" +
    "padding:4px 6px;border-radius:8px;background:var(--color-card,#1c1c1f);" +
    "border:1px solid var(--color-border,#3f3f46);box-shadow:0 4px 14px rgba(0,0,0,.35);";
  const input = document.createElement("input");
  input.type = "text";
  input.placeholder = zh ? "查找…" : "Find…";
  input.style.cssText =
    "width:180px;padding:3px 8px;border-radius:6px;border:1px solid var(--color-border,#3f3f46);" +
    "background:var(--color-input,#131316);color:var(--color-foreground,#f4f4f5);font-size:12px;outline:none;";
  const button = (label: string, title: string, action: () => void) => {
    const el = document.createElement("button");
    el.type = "button";
    el.textContent = label;
    el.title = title;
    el.style.cssText =
      "min-width:24px;height:24px;border-radius:6px;border:none;cursor:pointer;" +
      "background:var(--color-muted,#27272a);color:var(--color-foreground,#f4f4f5);font-size:12px;";
    el.addEventListener("click", action);
    return el;
  };
  const close = () => {
    bar.style.display = "none";
    search.clearDecorations();
    onClose();
  };
  bar.append(
    input,
    button("‹", zh ? "上一个" : "Previous", () => input.value && search.findPrevious(input.value)),
    button("›", zh ? "下一个" : "Next", () => input.value && search.findNext(input.value)),
    button("×", zh ? "关闭" : "Close", close),
  );
  input.addEventListener("keydown", (event) => {
    if (event.key === "Enter") {
      event.preventDefault();
      event.stopPropagation();
      if (input.value) (event.shiftKey ? search.findPrevious(input.value) : search.findNext(input.value));
    } else if (event.key === "Escape") {
      event.preventDefault();
      event.stopPropagation();
      close();
    }
  });
  container.appendChild(bar);
  return bar;
}

// Selection colors are injected into every theme: xterm's built-in fallback
// is a near-invisible white glaze (rgba(255,255,255,.3)) on light
// backgrounds. Both focus states deliberately share ONE color: the DBX
// webview's focus model can leave the terminal permanently "inactive", and
// a faded selection there looks exactly like the missing-selection bug this
// fixes. #rrggbbaa hits xterm's direct-parse branch (length 9), skipping
// the rgba() regex entirely. selectionForeground stays unset so colored
// log text keeps its ANSI colors inside the selection.
function withSelectionColors(theme: ITheme, light: boolean): ITheme {
  return {
    ...theme,
    selectionBackground: light ? "#2563eb66" : "#60a5fa73",
  };
}

// themeFor resolves a scheme id into an xterm theme; "auto" (or an unknown
// id) follows the DBX host tokens.
export function themeFor(schemeId: string): ITheme {
  const scheme = findScheme(schemeId);
  if (scheme) return withSelectionColors(scheme.theme, scheme.light === true);
  const light = isLightTheme();
  return withSelectionColors(
    light
      ? {
          background: tokenColor("--color-background", "#ffffff"),
          foreground: tokenColor("--color-foreground", "#27272a"),
          cursor: tokenColor("--color-primary", "#2563eb"),
        }
      : {
          background: tokenColor("--color-background", "#18181b"),
          foreground: tokenColor("--color-foreground", "#f4f4f5"),
          cursor: tokenColor("--color-primary", "#60a5fa"),
        },
    light,
  );
}

export function createTerminal(
  container: HTMLElement,
  schemeId = "auto",
  options: TerminalOptions = {},
): TerminalHandle {
  const style = { ...defaultTerminalStyle(), ...options.style };
  const resolveFont = (custom: string): string =>
    custom ||
    tokenColor("--font-mono", "") ||
    '"Cascadia Mono", Consolas, "Courier New", monospace';
  const terminal = new Terminal({
    fontFamily: resolveFont(style.fontFamily),
    fontSize: style.fontSize,
    cursorBlink: true,
    allowProposedApi: true,
    scrollback: style.scrollback,
    theme: themeFor(schemeId),
  });
  const fitAddon = new FitAddon();
  terminal.loadAddon(fitAddon);
  terminal.loadAddon(new WebLinksAddon());
  const searchAddon = new SearchAddon();
  terminal.loadAddon(searchAddon);
  terminal.open(container);

  // GPU rendering for high-throughput output; anything unsupported (no
  // WebGL context, exotic environment) falls back to the default renderer.
  // The preview-only ?renderer=dom switch forces that fallback — headless
  // screenshot harnesses paint DOM reliably where GPU surfaces may not.
  if (document.documentElement.dataset.noWebgl !== "1") {
    try {
      const webgl = new WebglAddon();
      webgl.onContextLoss(() => webgl.dispose());
      terminal.loadAddon(webgl);
    } catch {
      /* keep the DOM/canvas renderer */
    }
  }

  // Absolute positioning for the search bar needs a positioned ancestor.
  if (getComputedStyle(container).position === "static") {
    container.style.position = "relative";
  }
  const searchBar = buildSearchBar(container, searchAddon, () => terminal.focus());
  const openSearch = (): void => {
    searchBar.style.display = "flex";
    const input = searchBar.querySelector<HTMLInputElement>("input")!;
    input.focus();
    input.select();
  };
  terminal.attachCustomKeyEventHandler((event) => {
    if (event.type === "keydown" && (event.ctrlKey || event.metaKey) && !event.altKey && event.key.toLowerCase() === "f") {
      event.preventDefault();
      openSearch();
      return false;
    }
    return true;
  });

  // cwd reports: OSC 7 (our injected hooks, zsh/fish defaults) and OSC 9;9
  // (Windows conhost/PSReadLine). Returning false lets xterm keep its own
  // (non-rendering) handling — OSC sequences never reach the screen.
  if (options.onCwd) {
    const onCwd = options.onCwd;
    const forward = (data: string): boolean => {
      const path = oscToPath(data);
      if (path) onCwd(path);
      return false;
    };
    terminal.parser.registerOscHandler(7, forward);
    terminal.parser.registerOscHandler(9, forward);
  }

  try {
    fitAddon.fit();
  } catch {
    /* container not measured yet; the first resize observer tick will fit */
  }
  return {
    terminal,
    fit: () => {
      try {
        fitAddon.fit();
      } catch {
        /* ignore zero-size fits */
      }
    },
    write: (data) => terminal.write(data instanceof Uint8Array ? new Uint8Array(data) : data),
    onData: (handler) => terminal.onData(handler),
    reset: () => terminal.reset(),
    dispose: () => {
      searchBar.remove();
      terminal.dispose();
    },
    cols: () => terminal.cols,
    rows: () => terminal.rows,
    openSearch,
    setScheme: (schemeId: string) => {
      terminal.options.theme = themeFor(schemeId);
    },
    applyStyle: (next: TerminalStyle) => {
      terminal.options.fontFamily = resolveFont(next.fontFamily);
      terminal.options.fontSize = next.fontSize;
      terminal.options.scrollback = next.scrollback;
      try {
        fitAddon.fit();
      } catch {
        /* ignore zero-size fits */
      }
    },
  };
}
