// Sidecar RPC: typed invoke helpers plus the binary-channel and event demux
// shared by every view in this iframe.
import { bridge } from "./host";

export interface ConnectionInfo {
  connectionId: string;
  shellId: string;
  kind: string;
  name: string;
  host: string;
  username: string;
}

export interface ShellInfo extends ConnectionInfo {
  closed: boolean;
  createdAt: number;
  primary: boolean;
}

export interface DirEntry {
  name: string;
  size: number;
  isDir: boolean;
  isSymlink: boolean;
  mode?: string;
  modTime: number;
}

const binaryHandlers = new Map<string, Set<(data: Uint8Array) => void>>();
const eventHandlers = new Set<(message: { method: string; params?: any }) => void>();
let muxInstalled = false;

export function installMux(): void {
  if (muxInstalled) return;
  muxInstalled = true;
  bridge().onBinary(({ channel, data }) => {
    binaryHandlers.get(channel)?.forEach((handler) => handler(data));
  });
  bridge().onEvent((message) => {
    eventHandlers.forEach((handler) => handler(message));
  });
}

export function onBinaryChannel(channel: string, handler: (data: Uint8Array) => void): () => void {
  let set = binaryHandlers.get(channel);
  if (!set) {
    set = new Set();
    binaryHandlers.set(channel, set);
  }
  set.add(handler);
  return () => set.delete(handler);
}

export function onSidecarEvent(handler: (message: { method: string; params?: any }) => void): () => void {
  eventHandlers.add(handler);
  return () => eventHandlers.delete(handler);
}

export async function api<T = any>(method: string, params?: unknown, timeoutMs?: number): Promise<T> {
  try {
    return (await bridge().invoke(method, params ?? {}, timeoutMs ? { timeoutMs } : undefined)) as T;
  } catch (error: any) {
    throw new Error(error?.message || String(error));
  }
}

export function notify(method: string, params?: unknown): void {
  bridge().notify(method, params ?? {}).catch(() => undefined);
}

export function sendBinary(channel: string, data: Uint8Array): void {
  bridge().sendBinary(channel, data).catch(() => undefined);
}

// ---- typed helpers ----

export const WORKBENCH_ID = "com.milii.termhive.workbench";
export const MINI_ID = "com.milii.termhive.mini";

export async function listShells(): Promise<ShellInfo[]> {
  return api<ShellInfo[]>("session/shells");
}

// ensureConnection returns the connection's primary shell, dialing it when the
// sidecar has no live session (first open after connect, reconnect, backend
// restart).
export async function ensureConnection(connectionId: string): Promise<ConnectionInfo> {
  return api<ConnectionInfo>("connection/ensure", { connectionId }, 120000);
}

export async function newShellForConnection(
  connectionId: string,
  cols: number,
  rows: number,
): Promise<ConnectionInfo> {
  return api<ConnectionInfo>("session/newShell", { connectionId, cols, rows }, 120000);
}

export async function openSessionTab(context: {
  shellId?: string;
  connectionId: string;
  name: string;
  sub?: string;
  openFiles?: boolean;
}): Promise<void> {
  await bridge().openWorkbench(WORKBENCH_ID, { view: "session", ...context });
}

export function humanSize(size: number): string {
  if (!Number.isFinite(size) || size < 0) return "";
  if (size < 1024) return `${size} B`;
  const units = ["KB", "MB", "GB", "TB"];
  let value = size / 1024;
  let index = 0;
  while (value >= 1024 && index < units.length - 1) {
    value /= 1024;
    index += 1;
  }
  return `${value >= 100 ? value.toFixed(0) : value.toFixed(1)} ${units[index]}`;
}

export function joinRemote(...parts: string[]): string {
  const joined = parts.join("/").replace(/\\/g, "/");
  // collapse "." and ".." client-side so navigation never accumulates
  // segments like /C:/Users/../../D:/dir
  const segments: string[] = [];
  for (const segment of joined.split("/")) {
    if (!segment || segment === ".") continue;
    if (segment === "..") {
      if (segments.length > 0 && segments[segments.length - 1] !== "..") segments.pop();
      continue;
    }
    segments.push(segment);
  }
  return "/" + segments.join("/");
}
