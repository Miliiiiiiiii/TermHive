// Typings and feature-safe wrappers around the DBX Host Bridge. The dev host
// implements only a subset; desktop-only namespaces (floating, fileTransfer,
// clipboard read) are probed before use so every view degrades gracefully.

export interface FloatingApi {
  open(options: {
    contributionId: string;
    title?: string;
    width?: number;
    height?: number;
    x?: number;
    y?: number;
    alwaysOnTop?: boolean;
    skipTaskbar?: boolean;
    resizable?: boolean;
    context?: Record<string, unknown>;
  }): Promise<{ windowId: string; reused: boolean }>;
  close(contributionId?: string): Promise<void>;
  beginDrag(): void;
  endDrag(options?: { snap?: boolean }): Promise<unknown>;
  setSize(width: number, height: number): Promise<void>;
}

export interface FileHandle {
  handleId: string;
  // the real host serializes the file name as `fileName` (verified against
  // the DBX binary); `name` is kept only for dev-host compatibility, and
  // `filePath`/`path` are accepted as a last resort
  fileName?: string;
  name?: string;
  size: number;
  contentType?: string;
  relativePath?: string;
}

// resolve a handle's display name from whichever field the host populated
export function handleFileName(handle: FileHandle): string {
  const direct = handle.fileName ?? handle.name;
  if (direct) return direct;
  const extra = handle as unknown as Record<string, unknown>;
  const path = extra.filePath ?? extra.path;
  if (typeof path === "string" && path) {
    return path.replace(/\\/g, "/").split("/").filter(Boolean).pop() ?? "";
  }
  return "";
}

// resolve a handle's size in bytes, or null when the host did not report it
export function handleFileSize(handle: FileHandle): number | null {
  const size = handle.size ?? (handle as unknown as Record<string, unknown>).sizeBytes;
  return typeof size === "number" && Number.isFinite(size) ? size : null;
}

export interface FileTransferApi {
  // the real host resolves pick with a { files: FileHandle[] } wrapper (the
  // handles inside use the classic name/handleId/size fields); a bare
  // handle/array is accepted for dev-host compatibility
  pick(options?: { multiple?: boolean }): Promise<
    FileHandle | FileHandle[] | { files: FileHandle[] } | null
  >;
  read(handleId: string, offset: number, length?: number): Promise<{ dataBase64: string; length: number; eof: boolean }>;
  beginSave(options: { name: string; contentType?: string; size?: number }): Promise<{ handleId: string; chunkBytes: number } | null>;
  write(handleId: string, offset: number, data: unknown): Promise<{ written: number; nextOffset: number }>;
  finish(handleId: string): Promise<void>;
  cancel(handleId: string): Promise<void>;
  onDrop(fn: (files: FileHandle[], meta: { dropId: string; truncated: boolean }) => void): () => void;
  onDragState(fn: (active: boolean) => void): () => void;
}

export interface DbxPluginBridge {
  ready: Promise<Record<string, unknown>>;
  readonly context: Record<string, unknown>;
  readonly locale: string;
  readonly theme?: { appearance?: string };
  request(method: string, params?: unknown): Promise<unknown>;
  invoke(method: string, params?: unknown, options?: { timeoutMs?: number }): Promise<unknown>;
  notify(method: string, params?: unknown): Promise<unknown>;
  sendBinary(channel: string, data: Uint8Array | string): Promise<unknown>;
  onBinary(fn: (message: { channel: string; data: Uint8Array }) => void): () => void;
  onEvent(fn: (message: { method: string; params?: Record<string, unknown> }) => void): () => void;
  onContext(fn: (context: Record<string, unknown>) => void): () => void;
  onInit(fn: (context: Record<string, unknown>) => void): () => void;
  openWorkbench(contributionId: string, context?: Record<string, unknown>): Promise<unknown>;
  readAsset?(path: string): Promise<unknown>;
  copy?(text: string): Promise<unknown>;
  clipboard?: { writeText?(text: string): Promise<unknown>; readText?(): Promise<string> };
  setTitle?(title: string | null): Promise<unknown>;
  closeWorkbench?(): Promise<unknown>;
  floating?: FloatingApi;
  fileTransfer?: FileTransferApi;
  encodeBase64?(data: Uint8Array | string): string;
  decodeBase64?(base64: string): Uint8Array;
}

declare global {
  interface Window {
    dbxPlugin: DbxPluginBridge;
  }
}

export const bridge = (): DbxPluginBridge => window.dbxPlugin;

// Copy through the best available channel: the host bridge first (it owns
// the real clipboard in the sandboxed webview), then the async clipboard
// API, then a hidden-textarea execCommand fallback. Returns false only when
// every channel refused, so callers can suggest exporting to a file — a
// denied clipboard must not fail silently.
export async function copyToClipboard(text: string): Promise<boolean> {
  const plugin = window.dbxPlugin;
  try {
    if (typeof plugin?.copy === "function") {
      await plugin.copy(text);
      return true;
    }
  } catch {
    /* fall through to the next channel */
  }
  try {
    if (navigator.clipboard?.writeText) {
      await navigator.clipboard.writeText(text);
      return true;
    }
  } catch {
    /* fall through */
  }
  try {
    const helper = document.createElement("textarea");
    helper.value = text;
    helper.style.cssText = "position:fixed;left:-9999px;top:0;opacity:0;";
    document.body.append(helper);
    helper.select();
    const ok = document.execCommand("copy");
    helper.remove();
    if (ok) return true;
  } catch {
    /* all channels failed */
  }
  return false;
}

// Best-effort clipboard read for terminal paste menus; null when the host
// environment withholds the clipboard (permissions UI, sandbox policy).
export async function readClipboard(): Promise<string | null> {
  const plugin = window.dbxPlugin;
  try {
    if (typeof plugin?.clipboard?.readText === "function") return await plugin.clipboard.readText();
  } catch {
    /* fall through */
  }
  try {
    if (navigator.clipboard?.readText) return await navigator.clipboard.readText();
  } catch {
    /* denied */
  }
  return null;
}

export function hasFloating(): boolean {
  return typeof bridge().floating?.open === "function";
}

export function hasFileTransfer(): boolean {
  return typeof bridge().fileTransfer?.pick === "function";
}
