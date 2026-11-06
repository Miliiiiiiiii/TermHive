// rz/sz (ZMODEM) file transfers over the terminal stream.
//
// A Sentry sits in front of each tab's terminal output: bytes that look like
// a ZMODEM session (the user typed `rz` or `sz` on the server) are peeled
// off into a transfer — everything else flows through untouched, and it does
// so BEFORE the highlighter, because protocol frames must never be
// ANSI-re-encoded. Uploading (`rz`) reads host-picked files chunk by chunk;
// downloading (`sz`) streams offer payloads into a host save handle. The
// transfer queue UI is not involved in v1 — start/end/failure surface as
// notifications.
import * as ZmodemModule from "zmodem.js";
import { bridge, type FileHandle, handleFileName, handleFileSize } from "./host";

const Zmodem = ZmodemModule as unknown as { Sentry: any };

const READ_CHUNK = 64 * 1024;

export interface ZmodemHooks {
  /** passthrough for non-protocol bytes (the normal terminal path) */
  writeTerm(data: Uint8Array): void;
  /** bytes destined for the PTY (protocol frames) */
  sendToPty(data: Uint8Array): void;
  notify(text: string, kind?: "info" | "error"): void;
}

function toUint8(octets: number[] | Uint8Array | string): Uint8Array {
  if (octets instanceof Uint8Array) return octets;
  if (typeof octets === "string") return new TextEncoder().encode(octets);
  return Uint8Array.from(octets);
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

// createZmodemSentry builds the per-tab stream fork. consume() receives every
// terminal output chunk first; dispose aborts any live transfer.
export function createZmodemSentry(hooks: ZmodemHooks): {
  consume(data: Uint8Array): void;
  dispose(): void;
} {
  let session: any = null;
  let disposed = false;
  const plugin = bridge();

  const sentry = new Zmodem.Sentry({
    to_terminal: (octets: number[] | Uint8Array) => {
      const data = toUint8(octets);
      if (data.length > 0) hooks.writeTerm(data);
    },
    sender: (octets: number[] | Uint8Array) => {
      hooks.sendToPty(toUint8(octets));
    },
    on_retract: () => undefined,
    on_detect: (detection: any) => {
      let zsession: any;
      try {
        zsession = detection.confirm();
      } catch {
        detection.deny();
        return;
      }
      session = zsession;
      void runSession(zsession, hooks, plugin).finally(() => {
        session = null;
      });
    },
  });

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
  };
}

async function runSession(zsession: any, hooks: ZmodemHooks, plugin: ReturnType<typeof bridge>): Promise<void> {
  try {
    if (zsession.type === "send") {
      // `rz` on the server: it waits for us to send files.
      const files = await pickZmodemFiles();
      if (!files || files.length === 0) {
        hooks.notify("rz: 未选择文件，传输取消", "info");
        zsession.abort();
        return;
      }
      for (const handle of files) {
        const size = handleFileSize(handle) ?? 0;
        const name = handleFileName(handle);
        const xfer = await zsession.send_offer({
          name,
          size,
          mode: 0o644,
          mtime: Math.floor(Date.now() / 1000),
        });
        if (!xfer) continue; // peer skipped
        let offset = 0;
        while (offset < size) {
          const chunk = await readHandleChunk(handle, offset, Math.min(READ_CHUNK, size - offset));
          if (chunk.length === 0) break;
          xfer.send(chunk);
          offset += chunk.length;
        }
        await xfer.end();
        hooks.notify(`rz: ${name} 已上传`, "info");
      }
      await zsession.close();
      return;
    }
    // `sz` on the server: it sends us files.
    zsession.on("offer", (xfer: any) => {
      void (async () => {
        const details = xfer.get_details();
        const handle =
          typeof plugin.fileTransfer?.beginSave === "function"
            ? await plugin.fileTransfer.beginSave({ name: details.name, size: details.size })
            : null;
        if (!handle) {
          xfer.skip();
          hooks.notify(`sz: 已跳过 ${details.name}`, "info");
          return;
        }
        // writes must stay ordered even though input events fire in a tight
        // synchronous loop while the peer streams
        let writeChain = Promise.resolve();
        let received = 0;
        xfer.on("input", (octets: number[] | Uint8Array) => {
          const data = toUint8(octets);
          const offset = received;
          received += data.length;
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
        await xfer.accept();
        await writeChain;
        await plugin.fileTransfer!.finish(handle.handleId).catch(() => undefined);
        hooks.notify(`sz: ${details.name} 已保存`, "info");
      })();
    });
    zsession.on("session_end", () => undefined);
    await zsession.start();
  } catch (error: any) {
    hooks.notify(`ZMODEM 传输失败: ${error?.message || error}`, "error");
    try {
      zsession.abort();
    } catch {
      /* already gone */
    }
  }
}
