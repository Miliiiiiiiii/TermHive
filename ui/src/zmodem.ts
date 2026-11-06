// rz/sz (ZMODEM) file transfers over the terminal stream.
//
// A Sentry sits in front of each tab's terminal output: bytes that look like
// a ZMODEM session (the user typed `rz` or `sz` on the server) are peeled
// off into a transfer — everything else flows through untouched, and it does
// so BEFORE the highlighter, because protocol frames must never be
// ANSI-re-encoded.
//
// Progress UI (Xshell-style transfer window) is owned by the session view:
// this module only reports the live transfer list through onTransfers and
// accepts cancel()/dismiss() calls.
//
// Screen hygiene: zmodem.js's Sentry DELIBERATELY forwards the initial
// ZRQINIT/ZRINIT frame, any "garbage" it parses mid-session, and everything
// a peer keeps sending after the session died straight to to_terminal —
// with lrzsz that sprays hex noise across the screen that "never goes
// away". So the terminal path here swallows every block carrying the ZMODEM
// lead-in (`**\x18B`; \x18/CAN makes a false positive in real output
// effectively impossible) while a transfer is live or cooling down, and
// detections within the cooldown after a failed/canceled transfer are
// denied outright — deny() sends the ABORT sequence, so the server-side
// rz/sz exits cleanly instead of retrying for the next half minute.
import * as ZmodemModule from "zmodem.js";
import { bridge, type FileHandle, handleFileName, handleFileSize } from "./host";

const Zmodem = ZmodemModule as unknown as { Sentry: any };

const READ_CHUNK = 64 * 1024;
// how long after a transfer ends (or dies) to keep swallowing frames —
// lrzsz keeps re-sending headers for a while after an abort
const SUPPRESS_MS = 15000;

export interface ZmodemTransferItem {
  id: number;
  name: string;
  size: number;
  bytes: number;
  status: "active" | "done" | "failed" | "canceled";
}

export interface ZmodemHooks {
  /** passthrough for non-protocol bytes (the normal terminal path) */
  writeTerm(data: Uint8Array): void;
  /** bytes destined for the PTY (protocol frames) */
  sendToPty(data: Uint8Array): void;
  notify(text: string, kind?: "info" | "error"): void;
  /** forensic logging into the sidecar's diag.log */
  diag?(line: string): void;
  /** live transfer list for the progress window (throttled ~100ms) */
  onTransfers?(items: ZmodemTransferItem[]): void;
}

export interface ZmodemSentryHandle {
  consume(data: Uint8Array): void;
  dispose(): void;
  /** cancel one transfer: receive skips the file in-protocol, send aborts the session */
  cancel(id: number): void;
  /** drop a finished item from the reported list (auto-close housekeeping) */
  dismiss(id: number): void;
}

function toUint8(octets: number[] | Uint8Array | string): Uint8Array {
  if (octets instanceof Uint8Array) return octets;
  if (typeof octets === "string") return new TextEncoder().encode(octets);
  return Uint8Array.from(octets);
}

// "**\x18B" leads in every ZMODEM hex header
const ZMODEM_MARK = [42, 42, 24, 66];
function containsZmodemMark(data: Uint8Array): boolean {
  outer: for (let i = 0; i + ZMODEM_MARK.length <= data.length; i++) {
    for (let j = 0; j < ZMODEM_MARK.length; j++) {
      if (data[i + j] !== ZMODEM_MARK[j]) continue outer;
    }
    return true;
  }
  return false;
}

// pickZmodemFiles asks the host for one or more local files; returns null
// when the user cancels or nothing readable was picked.
async function pickZmodemFiles(): Promise<FileHandle[] | null> {
  const plugin = bridge();
  if (typeof plugin.fileTransfer?.pick !== "function") return null;
  const picked = await plugin.fileTransfer.pick({ multiple: true });
  if (!picked) return null;
  const files = Array.isArray(picked)
    ? picked
    : "files" in (picked as object)
      ? (picked as { files: FileHandle[] }).files
      : [picked as FileHandle];
  return files.filter((handle) => handleFileName(handle));
}

async function readHandleChunk(handle: FileHandle, offset: number, length: number): Promise<Uint8Array> {
  const plugin = bridge();
  const chunk = await plugin.fileTransfer!.read(handle.handleId, offset, length);
  return plugin.decodeBase64
    ? plugin.decodeBase64(chunk.dataBase64)
    : Uint8Array.from(atob(chunk.dataBase64), (c) => c.charCodeAt(0));
}

// createZmodemSentry builds the per-tab stream fork.
export function createZmodemSentry(hooks: ZmodemHooks): ZmodemSentryHandle {
  let session: any = null;
  let disposed = false;
  let suppressUntil = 0;
  // failed/canceled transfers deny fresh detections for one suppress window
  let denyUntil = 0;
  let suppressedBytes = 0;
  const plugin = bridge();
  const diag = (line: string): void => hooks.diag?.(line);
  const armCooldown = (): void => {
    denyUntil = Date.now() + SUPPRESS_MS;
  };

  // ---- transfer list (progress window feed) ----
  let transferSeq = 0;
  let items: ZmodemTransferItem[] = [];
  const offers = new Map<number, any>(); // receive-side xfer per item id
  let sendAbortRequested = false;
  let emitTimer = 0;
  const emit = (): void => {
    if (emitTimer) return;
    emitTimer = window.setTimeout(() => {
      emitTimer = 0;
      hooks.onTransfers?.([...items]);
    }, 100);
  };
  const upsert = (item: ZmodemTransferItem): void => {
    const index = items.findIndex((entry) => entry.id === item.id);
    if (index === -1) items.push(item);
    else items[index] = item;
    emit();
  };

  const sentry = new Zmodem.Sentry({
    to_terminal: (octets: number[] | Uint8Array) => {
      const data = toUint8(octets);
      if (data.length === 0) return;
      if (Date.now() < suppressUntil && containsZmodemMark(data)) {
        suppressedBytes += data.length;
        return;
      }
      hooks.writeTerm(data);
    },
    sender: (octets: number[] | Uint8Array) => {
      hooks.sendToPty(toUint8(octets));
    },
    on_retract: () => diag("detection retracted (peer bytes were not ZMODEM)"),
    on_detect: (detection: any) => {
      suppressUntil = Date.now() + SUPPRESS_MS;
      if (Date.now() < denyUntil) {
        diag("detection denied — cooldown after failed/canceled transfer");
        hooks.notify("ZMODEM: 上一次传输刚失败/取消，请稍候十几秒再重试", "info");
        detection.deny();
        return;
      }
      let zsession: any;
      try {
        zsession = detection.confirm();
      } catch (error) {
        diag("confirm failed: " + String(error));
        detection.deny();
        return;
      }
      session = zsession;
      sendAbortRequested = false;
      diag(`session confirmed role=${zsession.type}`);
      void runSession(zsession)
        .catch((error: any) => diag("session error: " + String(error?.message ?? error)))
        .finally(() => {
          session = null;
          suppressUntil = Date.now() + SUPPRESS_MS;
          if (suppressedBytes > 0) {
            diag(`suppressed ${suppressedBytes} bytes of ZMODEM noise`);
            suppressedBytes = 0;
          }
        });
    },
  });

  async function runSession(zsession: any): Promise<void> {
    try {
      if (zsession.type === "send") {
        // `rz` on the server: it waits for us to send files.
        const files = await pickZmodemFiles();
        if (!files || files.length === 0) {
          hooks.notify("rz: 未选择文件，传输取消", "info");
          diag("send: no files picked, aborting");
          armCooldown();
          zsession.abort();
          return;
        }
        for (const handle of files) {
          const size = handleFileSize(handle) ?? 0;
          const name = handleFileName(handle);
          const item: ZmodemTransferItem = { id: ++transferSeq, name, size, bytes: 0, status: "active" };
          upsert(item);
          const xfer = await zsession.send_offer({
            name,
            size,
            mode: 0o644,
            mtime: Math.floor(Date.now() / 1000),
          });
          if (!xfer) {
            upsert({ ...item, status: "canceled" });
            continue; // peer skipped
          }
          let offset = 0;
          while (offset < size) {
            if (sendAbortRequested) break;
            const chunk = await readHandleChunk(handle, offset, Math.min(READ_CHUNK, size - offset));
            if (chunk.length === 0) break;
            xfer.send(chunk);
            offset += chunk.length;
            upsert({ ...item, bytes: offset });
          }
          if (sendAbortRequested) {
            upsert({ ...item, status: "canceled", bytes: offset });
            zsession.abort();
            return;
          }
          await xfer.end();
          upsert({ ...item, status: "done", bytes: size });
          diag(`send: ${name} complete (${size} bytes)`);
        }
        await zsession.close();
        return;
      }
      // `sz` on the server: it sends us files.
      zsession.on("offer", (xfer: any) => {
        void (async () => {
          const details = xfer.get_details();
          const item: ZmodemTransferItem = {
            id: ++transferSeq,
            name: String(details.name ?? "file"),
            size: Number(details.size ?? 0),
            bytes: 0,
            status: "active",
          };
          offers.set(item.id, xfer);
          upsert(item);
          const handle =
            typeof plugin.fileTransfer?.beginSave === "function"
              ? await plugin.fileTransfer.beginSave({ name: item.name, size: item.size })
              : null;
          if (!handle) {
            offers.delete(item.id);
            upsert({ ...item, status: "canceled" });
            diag(`receive: skipped ${item.name} (no save handle)`);
            xfer.skip();
            return;
          }
          // writes must stay ordered even though input events fire in a tight
          // synchronous loop while the peer streams
          let writeChain = Promise.resolve();
          let received = 0;
          let canceled = false;
          xfer.on("input", (octets: number[] | Uint8Array) => {
            const data = toUint8(octets);
            const offset = received;
            received += data.length;
            upsert({ ...item, bytes: received });
            writeChain = writeChain.then(async () => {
              try {
                await plugin.fileTransfer!.write(
                  handle.handleId,
                  offset,
                  plugin.encodeBase64 ? plugin.encodeBase64(data) : (data as unknown),
                );
              } catch {
                /* the host surface is gone — the transfer will fail on its own */
              }
            });
          });
          // a skip() requested from the progress window flips this via the
          // offers map; accept() then resolves early
          canceled = items.find((entry) => entry.id === item.id)?.status === "canceled";
          try {
            await xfer.accept();
          } catch {
            canceled = true;
          }
          await writeChain;
          await plugin.fileTransfer!.finish(handle.handleId).catch(() => undefined);
          offers.delete(item.id);
          const finalStatus = canceled || items.find((entry) => entry.id === item.id)?.status === "canceled" ? "canceled" : "done";
          upsert({ ...item, status: finalStatus, bytes: received });
          diag(`receive: ${item.name} ${finalStatus} (${received} bytes)`);
        })();
      });
      zsession.on("session_end", () => diag("receive: session ended"));
      await zsession.start();
    } catch (error: any) {
      hooks.notify(`ZMODEM 传输失败: ${error?.message || error}`, "error");
      diag("session failed: " + String(error?.message ?? error));
      armCooldown();
      try {
        zsession.abort();
      } catch {
        /* already gone */
      }
    }
  }

  return {
    consume(data: Uint8Array) {
      if (disposed) {
        hooks.writeTerm(data);
        return;
      }
      sentry.consume(data);
    },
    dispose() {
      disposed = true;
      try {
        session?.abort?.();
      } catch {
        /* already gone */
      }
    },
    cancel(id: number) {
      const item = items.find((entry) => entry.id === id);
      if (!item || item.status !== "active") return;
      diag(`cancel requested for ${item.name}`);
      if (session?.type === "receive") {
        // in-protocol skip: the server drops this file and offers the next
        upsert({ ...item, status: "canceled" });
        try {
          offers.get(id)?.skip();
        } catch {
          /* offer already gone */
        }
        return;
      }
      // send: stop the read loop, mark everything in flight canceled, abort
      sendAbortRequested = true;
      for (const entry of items) {
        if (entry.status === "active") upsert({ ...entry, status: "canceled" });
      }
      try {
        session?.abort();
      } catch {
        /* already gone */
      }
      armCooldown();
    },
    dismiss(id: number) {
      items = items.filter((entry) => entry.id !== id);
      emit();
    },
  };
}
