// Remote file panel: directory browser plus the transfer queue. Uploads read
// local bytes either from host fileTransfer handles (desktop) or plain File
// objects (browser dev host) and stream them over the up/<transferId> binary
// channel; downloads mirror that over down/<transferId>.
import { bridge, handleFileName, handleFileSize, hasFileTransfer, type FileHandle } from "./host";
import { icon } from "./icon";
import { t } from "./i18n";
import { notify, confirmBox, promptBox } from "./notify";
import { createTerminal } from "./term";
import { Highlighter } from "./highlight";
import { loadStyleSettings, onStyleSettingsChanged } from "./styleSettings";
import {
  api,
  notify as rpcNotify,
  onBinaryChannel,
  onSidecarEvent,
  sendBinary,
  humanSize,
  joinRemote,
  type DirEntry,
} from "./rpc";

interface QueueItem {
  id: string;
  name: string;
  direction: "up" | "down";
  size: number;
  bytes: number;
  status: "active" | "done" | "failed" | "canceled";
  error?: string;
  transferId?: string;
  cancel?: () => void;
  row: HTMLElement;
  bar: HTMLElement;
  label: HTMLElement;
  autoCloseScheduled?: boolean;
}

const UPLOAD_CHUNK = 256 * 1024;

// FilePanel is the per-tab remote browser; followCwd feeds it the terminal's
// current directory (OSC 7 shell integration) so the follow toggle can react
// instantly instead of polling.
export interface FilePanel {
  root: HTMLElement;
  followCwd(path: string): void;
}

// startDir: when the terminal already reported its directory, the panel is
// BORN there (no home round trip) — otherwise the initial home listing races
// the follow jump and, being two round trips slower to START but finishing
// around it, could overwrite the jumped directory right after it rendered.
export function createFilePanel(shellId: string, startDir?: string): FilePanel {
  let cwd = startDir ?? "";
  const root = document.createElement("div");
  root.className = "fp";
  root.innerHTML = `
    <div class="fp-bar">
      <button class="icon-btn" data-act="home" title="Home">${icon("home")}</button>
      <button class="icon-btn" data-act="up" title="..">${icon("levelUp")}</button>
      <button class="icon-btn" data-act="refresh" title="${t("refresh")}">${icon("refresh")}</button>
      <button class="icon-btn active" data-act="follow" id="fp-follow" title="${t("followDir")}">${icon("locate")}</button>
      <input class="fp-path" spellcheck="false" />
      <button class="icon-btn" data-act="mkdir" title="${t("mkdir")}">${icon("folderPlus")}</button>
      <button class="btn small ghost" data-act="downloadSel">${icon("download", 13)}${t("downloadSelected")}<span data-role="count"></span></button>
      <button class="btn small ghost" data-act="uploadFile">${icon("upload", 13)}${t("uploadFile")}</button>
      <button class="btn small" data-act="uploadFolder">${icon("folder", 13)}${t("uploadFolder")}</button>
    </div>
    <div class="fp-list"></div>
    <div class="fp-queue"></div>
    <div class="fp-drop" hidden><span>${t("dropHere")}</span></div>
    <input type="file" multiple hidden class="fp-input-file" />
    <input type="file" webkitdirectory hidden class="fp-input-dir" />
  `;
  const pathInput = root.querySelector<HTMLInputElement>(".fp-path")!;
  const listBox = root.querySelector<HTMLElement>(".fp-list")!;
  const queueBox = root.querySelector<HTMLElement>(".fp-queue")!;
  const dropOverlay = root.querySelector<HTMLElement>(".fp-drop")!;
  const inputFile = root.querySelector<HTMLInputElement>(".fp-input-file")!;
  const inputDir = root.querySelector<HTMLInputElement>(".fp-input-dir")!;
  const followButton = root.querySelector<HTMLElement>("#fp-follow")!;

  const queue: QueueItem[] = [];

  // finished-transfer rows auto-dismiss after the configured delay (plugin
  // setting, live-synced); failed/canceled rows always stay for inspection
  let transferClose = { enabled: true, delayMs: 3000 };
  void loadStyleSettings().then((settings) => {
    transferClose = {
      enabled: settings.transferAutoClose,
      delayMs: settings.transferAutoCloseDelay * 1000,
    };
  });
  onStyleSettingsChanged((settings) => {
    transferClose = {
      enabled: settings.transferAutoClose,
      delayMs: settings.transferAutoCloseDelay * 1000,
    };
  });

  // ---- queue rendering ----
  function makeQueueItem(name: string, direction: "up" | "down", size: number): QueueItem {
    const row = document.createElement("div");
    row.className = "q-item";
    row.innerHTML = `
      <span class="q-dir">${icon(direction === "up" ? "upload" : "download", 13)}</span>
      <span class="q-name"></span>
      <div class="q-progress"><div class="q-bar"></div></div>
      <span class="q-label">0%</span>
      <button class="icon-btn" style="width:22px;height:22px">${icon("close", 12)}</button>
    `;
    row.querySelector<HTMLElement>(".q-name")!.textContent = name;
    const item: QueueItem = {
      id: `${direction}-${Date.now()}-${Math.random().toString(36).slice(2, 7)}`,
      name,
      direction,
      size,
      bytes: 0,
      status: "active",
      row,
      bar: row.querySelector<HTMLElement>(".q-bar")!,
      label: row.querySelector<HTMLElement>(".q-label")!,
    };
    row.querySelector("button")!.addEventListener("click", () => {
      if (item.status === "active") item.cancel?.();
      else {
        removeQueueRow(row);
      }
    });
    queueBox.append(row);
    queue.push(item);
    return item;
  }

  // rows leave with a fade instead of vanishing: a bar that just showed 100%
  // blinking out reads as "something went wrong"
  const QUEUE_FADE_MS = 260;
  function removeQueueRow(row: HTMLElement): void {
    if (!row.isConnected || row.classList.contains("closing")) return;
    row.classList.add("closing");
    window.setTimeout(() => row.remove(), QUEUE_FADE_MS);
  }

  function updateItem(item: QueueItem): void {
    const percent = item.size > 0 ? Math.min(100, Math.round((item.bytes / item.size) * 100)) : 100;
    item.bar.style.width = `${percent}%`;
    item.label.textContent =
      item.status === "active"
        ? `${percent}% · ${humanSize(item.bytes)}/${humanSize(item.size)}`
        : item.status === "done"
          ? t("done")
          : item.status === "canceled"
            ? t("canceled")
            : `${t("failed")}: ${item.error ?? ""}`;
    item.row.classList.toggle("done", item.status !== "active");
    item.row.classList.toggle("failed", item.status === "failed");
    // schedule exactly once, on the transition into done — later updates
    // (final byte counts) must not stack extra timers
    if (item.status === "done" && !item.autoCloseScheduled) {
      item.autoCloseScheduled = true;
      if (transferClose.enabled) {
        window.setTimeout(() => {
          if (item.status === "done" && item.row.isConnected) removeQueueRow(item.row);
        }, Math.max(0, transferClose.delayMs));
      }
    }
  }

  // ---- browsing ----
  const selection = new Set<string>();
  const downloadSelButton = root.querySelector<HTMLElement>('[data-act="downloadSel"]')!;
  const downloadSelCount = downloadSelButton.querySelector<HTMLElement>("[data-role=count]")!;
  let lastEntries: DirEntry[] = [];

  function updateSelectionUI(): void {
    downloadSelButton.hidden = selection.size === 0;
    downloadSelCount.textContent = selection.size > 0 ? ` (${selection.size})` : "";
  }
  updateSelectionUI();

  // loadSeq stale-guards every listing: navigations can overlap (follow jumps
  // while a manual listing is still in flight, panel-open initial load vs the
  // first OSC report) and only the LATEST request may render — the losing
  // response arriving later must never overwrite the newer directory.
  let loadSeq = 0;
  async function load(path?: string): Promise<void> {
    const seq = ++loadSeq;
    try {
      if (path === undefined) {
        if (!cwd) {
          const home = await api<{ path: string }>("fs/home", { shellId });
          if (seq !== loadSeq) return;
          cwd = home.path;
        }
        path = cwd;
      }
      const result = await api<{ path: string; entries: DirEntry[] }>("fs/list", { shellId, path }, 60000);
      if (seq !== loadSeq) return;
      cwd = result.path; // normalized by the backend; never accumulates ".."
      pathInput.value = cwd;
      selection.clear();
      updateSelectionUI();
      renderList(result.entries);
    } catch (error: any) {
      if (seq === loadSeq) renderError(error.message);
    }
  }

  function renderError(message: string): void {
    listBox.innerHTML = "";
    const empty = document.createElement("div");
    empty.className = "fp-empty";
    empty.textContent = message;
    listBox.append(empty);
  }

  function renderList(entries: DirEntry[]): void {
    lastEntries = entries;
    listBox.innerHTML = "";
    if (!entries.length) {
      const empty = document.createElement("div");
      empty.className = "fp-empty";
      empty.innerHTML = `${icon("folder", 22)}<span>${t("empty")}</span>`;
      listBox.append(empty);
      return;
    }
    const table = document.createElement("table");
    table.className = "fp-table";
    table.innerHTML = "<thead><tr><th class=\"fp-check\"><input type=\"checkbox\" data-role=\"select-all\" /></th><th></th><th>名称</th><th>大小</th><th>修改时间</th><th></th></tr></thead>";
    const body = document.createElement("tbody");
    const selectAll = table.querySelector<HTMLInputElement>("[data-role=select-all]")!;
    selectAll.addEventListener("change", () => {
      if (selectAll.checked) {
        for (const entry of entries) selection.add(entry.name);
      } else {
        selection.clear();
      }
      body.querySelectorAll<HTMLInputElement>(".fp-check input").forEach((box) => {
        box.checked = selectAll.checked;
      });
      updateSelectionUI();
    });
    for (const entry of entries) {
      const row = document.createElement("tr");
      row.innerHTML = `
        <td class="fp-check"><input type="checkbox" ${selection.has(entry.name) ? "checked" : ""} /></td>
        <td class="fp-icon"></td>
        <td class="fp-name"></td>
        <td class="fp-size"></td>
        <td class="fp-time"></td>
        <td class="fp-actions"></td>
      `;
      row.querySelector<HTMLInputElement>(".fp-check input")!.addEventListener("change", (event) => {
        event.stopPropagation();
        if ((event.target as HTMLInputElement).checked) selection.add(entry.name);
        else selection.delete(entry.name);
        updateSelectionUI();
      });
      row.querySelector<HTMLElement>(".fp-icon")!.innerHTML = entry.isDir
        ? icon("folder", 15)
        : entry.isSymlink
          ? icon("external", 15)
          : icon("files", 15);
      row.querySelector<HTMLElement>(".fp-name")!.textContent = entry.name;
      row.querySelector<HTMLElement>(".fp-size")!.textContent = entry.isDir ? "" : humanSize(entry.size);
      row.querySelector<HTMLElement>(".fp-time")!.textContent = entry.modTime
        ? new Date(entry.modTime).toLocaleString()
        : "";
      const actions = row.querySelector<HTMLElement>(".fp-actions")!;
      const download = document.createElement("button");
      download.className = "icon-btn";
      download.innerHTML = icon("download", 14);
      download.title = t("download");
      download.addEventListener("click", () => downloadEntry(entry));
      if (!entry.isDir) {
        const edit = document.createElement("button");
        edit.className = "icon-btn";
        edit.innerHTML = icon("code", 14);
        edit.title = t("editFile");
        edit.addEventListener("click", () => void openEditor(joinRemote(cwd, entry.name)));
        actions.append(edit);
        const follow = document.createElement("button");
        follow.className = "icon-btn";
        follow.innerHTML = icon("follow", 14);
        follow.title = t("followLog");
        follow.addEventListener("click", () => openLogView(joinRemote(cwd, entry.name)));
        actions.append(follow);
      }
      const rename = document.createElement("button");
      rename.className = "icon-btn";
      rename.innerHTML = icon("pencil", 14);
      rename.title = t("rename");
      rename.addEventListener("click", async () => {
        const next = await promptBox(t("newName"), entry.name);
        if (!next || next === entry.name) return;
        try {
          await api("fs/rename", { shellId, from: joinRemote(cwd, entry.name), to: joinRemote(cwd, next) }, 60000);
          await load();
        } catch (error: any) {
          notify(error.message);
        }
      });
      const del = document.createElement("button");
      del.className = "icon-btn danger";
      del.innerHTML = icon("trash", 14);
      del.title = t("remove");
      del.addEventListener("click", async () => {
        if (!(await confirmBox(`${t("confirmDeleteFile")}: ${entry.name}`))) return;
        try {
          await api("fs/remove", { shellId, path: joinRemote(cwd, entry.name), recursive: entry.isDir }, 120000);
          await load();
        } catch (error: any) {
          notify(error.message);
        }
      });
      actions.append(download, rename, del);
      row.addEventListener("dblclick", () => {
        if (entry.isDir) void load(joinRemote(cwd, entry.name));
      });
      body.append(row);
    }
    table.append(body);
    listBox.append(table);
  }

  // ---- uploads ----
  interface LocalSource {
    name: string;
    size: number;
    relativePath?: string;
    read(offset: number, length: number): Promise<Uint8Array>;
    finish?(): Promise<void>;
    cancel?(): Promise<void>;
  }

  function handleSource(handle: FileHandle): LocalSource {
    const plugin = bridge();
    const size = handleFileSize(handle);
    return {
      name: handleFileName(handle),
      size: size ?? Number.NaN,
      relativePath: handle.relativePath,
      read: async (offset, length) => {
        const chunk = await plugin.fileTransfer!.read(handle.handleId, offset, length);
        return plugin.decodeBase64 ? plugin.decodeBase64(chunk.dataBase64) : Uint8Array.from(atob(chunk.dataBase64), (c) => c.charCodeAt(0));
      },
      finish: () => plugin.fileTransfer!.finish(handle.handleId).catch(() => undefined),
      cancel: () => plugin.fileTransfer!.cancel(handle.handleId).catch(() => undefined),
    };
  }

  function fileSource(file: File, relativePath?: string): LocalSource {
    return {
      name: file.name,
      size: file.size,
      relativePath,
      read: async (offset, length) => new Uint8Array(await file.slice(offset, offset + length).arrayBuffer()),
    };
  }

  async function uploadSources(sources: LocalSource[]): Promise<void> {
    // a handle without a usable name/size would upload onto the bare cwd or
    // an empty file — filter loudly, never silently
    const usable = sources.filter(
      (source) => (source.relativePath || source.name) && Number.isFinite(source.size) && source.size >= 0,
    );
    if (usable.length < sources.length) {
      const skipped = sources
        .filter((source) => !usable.includes(source))
        .map((source) => source.relativePath || source.name || "?")
        .join(", ");
      notify(`${t("pickUnreadable")}${skipped}`);
    }
    sources = usable;
    if (!sources.length) return;
    if (!cwd) await load();
    // Create every directory that appears in a relativePath first.
    const dirs = new Set<string>();
    for (const source of sources) {
      if (!source.relativePath?.includes("/")) continue;
      const parts = source.relativePath.split("/");
      parts.pop();
      let prefix = cwd;
      for (const part of parts) {
        prefix = joinRemote(prefix, part);
        dirs.add(prefix);
      }
    }
    for (const dir of dirs) {
      try {
        await api("fs/mkdirAll", { shellId, path: dir }, 120000);
      } catch {
        /* already exists */
      }
    }
    for (const source of sources) {
      const target = source.relativePath ? joinRemote(cwd, source.relativePath) : joinRemote(cwd, source.name);
      await uploadOne(source, target).catch(() => undefined);
    }
    await load();
  }

  function uploadOne(source: LocalSource, target: string): Promise<void> {
    const item = makeQueueItem(source.relativePath || source.name, "up", source.size);
    updateItem(item);
    let canceled = false;
    item.cancel = () => {
      canceled = true;
      if (item.transferId) rpcNotify("transfer/cancel", { transferId: item.transferId });
      source.cancel?.();
      item.status = "canceled";
      updateItem(item);
    };
    return (async () => {
      const { transferId } = await api<{ transferId: string }>("transfer/startUpload", { shellId, remotePath: target, size: source.size }, 60000);
      item.transferId = transferId;
      let offset = 0;
      while (offset < source.size && !canceled) {
        const chunk = await source.read(offset, Math.min(UPLOAD_CHUNK, source.size - offset));
        sendBinary(`up/${transferId}`, chunk);
        const event = await Promise.race([
          waitForTransferEvent(transferId, 120000),
        ]);
        if (event.method === "transfer.done") {
          if (!event.params.ok) throw new Error(event.params.error || "upload failed");
          offset = source.size;
          break;
        }
        offset = (event.params?.received as number) ?? offset + chunk.length;
        item.bytes = offset;
        updateItem(item);
      }
      if (canceled) return;
      if (item.bytes < source.size) {
        // ensure the done event has arrived before declaring success
        await waitForTransferDone(transferId, 120000);
        item.bytes = source.size;
      }
      await source.finish?.();
      item.status = "done";
      item.bytes = source.size;
      updateItem(item);
    })().catch((error) => {
      if (canceled) return;
      item.status = "failed";
      item.error = error.message;
      updateItem(item);
      source.cancel?.();
    });
  }

  function waitForTransferEvent(transferId: string, timeoutMs: number) {
    return new Promise<{ method: string; params: any }>((resolve, reject) => {
      const timer = setTimeout(() => {
        off();
        reject(new Error("传输超时"));
      }, timeoutMs);
      const off = onSidecarEvent((message) => {
        if (message.method !== "transfer.ack" && message.method !== "transfer.done") return;
        if (message.params?.transferId !== transferId) return;
        clearTimeout(timer);
        off();
        resolve(message as { method: string; params: any });
      });
    });
  }

  function waitForTransferDone(transferId: string, timeoutMs: number) {
    return new Promise<{ method: string; params: any }>((resolve, reject) => {
      const timer = setTimeout(() => {
        off();
        reject(new Error("传输超时"));
      }, timeoutMs);
      const off = onSidecarEvent((message) => {
        if (message.method !== "transfer.done" || message.params?.transferId !== transferId) return;
        clearTimeout(timer);
        off();
        if (!message.params.ok) reject(new Error(message.params.error || "upload failed"));
        else resolve(message as { method: string; params: any });
      });
    });
  }

  // ---- downloads ----
  async function downloadByName(name: string, remotePath: string, size: number): Promise<void> {
    const item = makeQueueItem(name, "down", size);
    updateItem(item);
    try {
      await streamDownload(item, size, name, () =>
        api<{ transferId: string; size: number }>(
          "transfer/startDownload",
          { shellId, remotePath },
          60000,
        ),
      );
    } catch (error: any) {
      item.status = "failed";
      item.error = error.message;
      updateItem(item);
    }
  }

  async function downloadEntry(entry: DirEntry): Promise<void> {
    await downloadByName(entry.name, joinRemote(cwd, entry.name), entry.size);
  }

  // Batch download: one zip over every selected entry, one save dialog.
  async function downloadSelected(): Promise<void> {
    const names = [...selection];
    if (!names.length) return;
    if (names.length === 1) {
      // single selection keeps the plain single-file flow
      const entry = lastEntries.find((item) => item.name === names[0]);
      if (entry && !entry.isDir) {
        selection.clear();
        updateSelectionUI();
        await downloadEntry(entry);
        return;
      }
    }
    const stamp = new Date().toISOString().slice(0, 16).replace(/[:T]/g, "");
    const zipName = `dbx-${stamp}.zip`;
    const item = makeQueueItem(zipName, "down", 0);
    updateItem(item);
    try {
      await streamDownload(item, 0, zipName, () =>
        api<{ transferId: string; size: number }>(
          "transfer/startDownloadZip",
          { shellId, paths: names.map((name) => joinRemote(cwd, name)) },
          300000,
        ),
      );
    } catch (error: any) {
      item.status = "failed";
      item.error = error.message;
      updateItem(item);
    }
  }

  // Shared pump for downloads. Order matters: the save dialog (when the host
  // provides one) runs BEFORE transfer/startDownload, and the backend keeps
  // its pump gated until transfer/ready — sent only after this side has
  // subscribed to down/<transferId> — so no chunk can be emitted while the
  // dialog is open into a demux that would drop it. Chunks land in the host
  // save handle (or a blob fallback) and acks flow back for flow control.
  function streamDownload(
    item: QueueItem,
    size: number,
    fallbackName: string,
    start: () => Promise<{ transferId: string; size: number }>,
  ): Promise<void> {
    const plugin = bridge();
    const canSave = typeof plugin.fileTransfer?.beginSave === "function";
    let handle: { handleId: string } | null = null;
    return (async () => {
      if (canSave) {
        handle = await plugin.fileTransfer!.beginSave({ name: fallbackName, size });
        if (!handle) {
          item.status = "canceled";
          updateItem(item);
          return;
        }
      }
      const started = await start();
      item.transferId = started.transferId;
      item.size = started.size || size;
      item.cancel = () => {
        rpcNotify("transfer/cancel", { transferId: started.transferId });
        item.status = "canceled";
        updateItem(item);
      };
      const chunks: Uint8Array[] = [];
      let received = 0;
      const offBinary = onBinaryChannel(`down/${started.transferId}`, async (data) => {
        try {
          if (handle) {
            await plugin.fileTransfer!.write(handle.handleId, received, plugin.encodeBase64 ? plugin.encodeBase64(data) : data);
          } else {
            chunks.push(data);
          }
          received += data.length;
          item.bytes = received;
          updateItem(item);
          rpcNotify("transfer/ackDown", { transferId: started.transferId });
        } catch (error: any) {
          item.status = "failed";
          item.error = error.message;
          updateItem(item);
          rpcNotify("transfer/cancel", { transferId: started.transferId });
        }
      });
      const done = new Promise<void>((resolve) => {
        const offEvent = onSidecarEvent((message) => {
          if (message.method !== "transfer.done" || message.params?.transferId !== started.transferId) return;
          offBinary();
          offEvent();
          if (!message.params.ok) {
            item.status = "failed";
            item.error = message.params.error;
            if (handle) plugin.fileTransfer!.cancel(handle.handleId).catch(() => undefined);
          } else {
            if (handle) {
              plugin.fileTransfer!.finish(handle.handleId).catch(() => undefined);
            } else {
              saveBlob(fallbackName, chunks);
            }
            item.status = "done";
            item.bytes = item.size || received;
          }
          updateItem(item);
          resolve();
        });
      });
      // the backend pump stays idle until this lands — both listeners are
      // registered by now, so the first chunk cannot race the subscription
      rpcNotify("transfer/ready", { transferId: started.transferId });
      await done;
    })().catch((error: any) => {
      item.status = "failed";
      item.error = error.message;
      updateItem(item);
      if (handle) plugin.fileTransfer!.cancel(handle.handleId).catch(() => undefined);
      if (item.transferId) rpcNotify("transfer/cancel", { transferId: item.transferId });
    });
  }

  function saveBlob(name: string, chunks: Uint8Array[]): void {
    const blob = new Blob(chunks as BlobPart[]);
    const url = URL.createObjectURL(blob);
    const anchor = document.createElement("a");
    anchor.href = url;
    anchor.download = name;
    anchor.click();
    setTimeout(() => URL.revokeObjectURL(url), 5000);
  }

  // ---- pickers ----
  async function pickFiles(): Promise<void> {
    const plugin = bridge();
    if (hasFileTransfer()) {
      try {
        const picked = await plugin.fileTransfer!.pick({ multiple: true });
        if (!picked) return;
        // unwrap the { files: [...] } wrapper the real host returns
        const handles = Array.isArray(picked)
          ? picked
          : Array.isArray((picked as { files?: FileHandle[] }).files)
            ? (picked as { files: FileHandle[] }).files
            : [picked as FileHandle];
        // forensic trail: dump the RAW shape the host returned, so any field
        // mismatch is provable from the diag log instead of guesswork
        void api(
          "diag/log",
          { line: `pick raw: ${JSON.stringify(picked).slice(0, 500)}` },
          5000,
        ).catch(() => undefined);
        // split by readability first: a handle without name AND size cannot
        // be uploaded — say so loudly instead of silently skipping it
        const readable = handles.filter(
          (handle) => handleFileName(handle) && handleFileSize(handle) !== null,
        );
        const unreadable = handles.filter(
          (handle) => !handleFileName(handle) || handleFileSize(handle) === null,
        );
        if (unreadable.length) {
          notify(
            t("pickUnreadable") +
              unreadable.map((handle) => JSON.stringify(handle).slice(0, 120)).join(" ; "),
          );
        }
        // the native picker happily hands back a selected folder as a
        // size-0 "file"; uploading that would target the directory itself.
        // There is no API to enumerate a picked folder, so route the user
        // to the folder upload instead of failing deep in the transfer.
        const folders = readable.filter((handle) => handleFileSize(handle) === 0);
        if (folders.length > 0) {
          const files = readable.filter((handle) => (handleFileSize(handle) ?? 0) > 0);
          notify(
            folders.map((handle) => `『${handleFileName(handle)}』`).join("") + t("pickedFolder"),
            "info",
          );
          if (files.length) await uploadSources(files.map(handleSource));
          await pickFolder();
          return;
        }
        if (readable.length) await uploadSources(readable.map(handleSource));
        return;
      } catch {
        /* fall back to the plain input */
      }
    }
    inputFile.click();
  }

  async function pickFolder(): Promise<void> {
    inputDir.click();
  }

  // ---- wiring ----
  root.querySelector<HTMLElement>(".fp-bar")!.addEventListener("click", async (event) => {
    const action = (event.target as HTMLElement).closest<HTMLElement>("[data-act]")?.dataset.act;
    if (!action) return;
    if (action === "home") {
      cwd = "";
      await load();
    } else if (action === "up") {
      if (cwd && cwd !== "/") await load(joinRemote(cwd, ".."));
    } else if (action === "refresh") {
      const btn = (event.target as HTMLElement).closest<HTMLElement>('[data-act="refresh"]');
      btn?.classList.add("spin");
      window.setTimeout(() => btn?.classList.remove("spin"), 450);
      await load();
    } else if (action === "follow") {
      // per-session switch: each terminal tab's panel keeps its own follow
      // state — turning it off on one session never touches another
      const on = followButton.classList.toggle("active");
      // the toast says WHERE follow would land — clicking the toggle when the
      // panel already sits in the terminal's directory otherwise looks like a
      // dead button, and a shell that never reported a directory becomes
      // visible instead of silently never following
      notify(on ? (terminalCwd ? `${t("followOn")} · ${terminalCwd}` : t("followPending")) : t("followOff"), "info");
      // enabling follow lands on wherever the terminal already is
      if (on && terminalCwd && terminalCwd !== cwd) void load(terminalCwd);
    } else if (action === "downloadSel") {
      await downloadSelected();
    } else if (action === "mkdir") {
      const name = await promptBox(t("newFolderName"));
      if (!name) return;
      try {
        await api("fs/mkdir", { shellId, path: joinRemote(cwd, name) }, 60000);
        await load();
      } catch (error: any) {
        notify(error.message);
      }
    } else if (action === "uploadFile") {
      await pickFiles();
    } else if (action === "uploadFolder") {
      await pickFolder();
    }
  });

  pathInput.addEventListener("keydown", async (event) => {
    if (event.key === "Enter") await load(pathInput.value.trim());
  });

  inputFile.addEventListener("change", async () => {
    const files = Array.from(inputFile.files ?? []);
    inputFile.value = "";
    await uploadSources(files.map((file) => fileSource(file)));
  });

  inputDir.addEventListener("change", async () => {
    const files = Array.from(inputDir.files ?? []);
    inputDir.value = "";
    await uploadSources(
      files.map((file) => fileSource(file, (file as any).webkitRelativePath || file.name)),
    );
  });

  // Host-level OS drag & drop (desktop) plus HTML5 drop as fallback. Every
  // session tab's panel registers here, and the host broadcasts one drop to
  // all of them — only the panel actually on screen may act on it, or one
  // drag uploads the same file into every tab's browsing directory.
  if (hasFileTransfer()) {
    bridge().fileTransfer!.onDrop((files) => {
      if (!root.isConnected) return;
      void uploadSources(
        files.map((handle) => handleSource(handle)).filter((source) => source.size >= 0),
      );
    });
  }
  root.addEventListener("dragover", (event) => {
    event.preventDefault();
    dropOverlay.hidden = false;
  });
  root.addEventListener("dragleave", () => {
    dropOverlay.hidden = true;
  });

  // HTML5 drop must walk directories via the entries API: dataTransfer.files
  // presents a dropped folder as a size-0 pseudo-file, and writing that path
  // remotely fails with SSH_FX_FAILURE when the directory already exists.
  async function collectDropFiles(
    transfer: DataTransfer,
  ): Promise<{ files: { file: File; path: string }[]; dirs: string[] }> {
    const out = { files: [] as { file: File; path: string }[], dirs: [] as string[] };
    const entries = Array.from(transfer.items ?? [])
      .map((item) => (typeof item.webkitGetAsEntry === "function" ? item.webkitGetAsEntry() : null))
      .filter(Boolean) as FileSystemEntry[];
    if (!entries.length) {
      for (const file of Array.from(transfer.files)) out.files.push({ file, path: file.name });
      return out;
    }
    const walk = async (entry: FileSystemEntry, prefix: string): Promise<void> => {
      if (entry.isFile) {
        const file = await new Promise<File>((resolve, reject) =>
          (entry as FileSystemFileEntry).file(resolve, reject),
        );
        out.files.push({ file, path: prefix + file.name });
        return;
      }
      const dirPath = prefix + entry.name;
      out.dirs.push(dirPath);
      const reader = (entry as FileSystemDirectoryEntry).createReader();
      for (;;) {
        const batch = await new Promise<FileSystemEntry[]>((resolve, reject) =>
          reader.readEntries(resolve as (v: FileSystemEntry[]) => void, reject),
        );
        if (!batch.length) break;
        for (const child of batch) await walk(child, `${dirPath}/`);
      }
    };
    for (const entry of entries) await walk(entry, "");
    return out;
  }

  root.addEventListener("drop", async (event) => {
    event.preventDefault();
    dropOverlay.hidden = true;
    if (!event.dataTransfer) return;
    const { files, dirs } = await collectDropFiles(event.dataTransfer);
    // create dropped directories that ended up empty (uploadSources only
    // mkdir-alls parents of actual files)
    for (const dir of dirs) {
      if (files.some((item) => item.path.startsWith(`${dir}/`))) continue;
      await api("fs/mkdirAll", { shellId, path: joinRemote(cwd, dir) }, 120000).catch(() => undefined);
    }
    if (files.length) {
      await uploadSources(
        files.map(({ file, path }) => fileSource(file, path === file.name ? undefined : path)),
      );
    }
  });

  // ---- follow the terminal's cwd ----
  // Driven by OSC 7 shell-integration events (see term.ts), not polling: a
  // separate exec channel can never see the interactive shell's directory —
  // every SSH session starts in the home directory — so the old 3s poll
  // compared home == home forever and the toggle did nothing. Events only
  // arrive when the shell renders a prompt, i.e. exactly when the directory
  // may have changed; manual browsing is untouched until the next cd.
  let terminalCwd: string | null = null;
  function followCwd(path: string): void {
    terminalCwd = path;
    if (!followButton.classList.contains("active")) return;
    if (!path || path === cwd) return;
    void load(path);
  }

  void load();

  // ---- in-app remote file editor ----
  // fs/read loads one window (≤2MB, transcoded to UTF-8 by the sidecar using
  // the connection's encoding hint). Files above the window size open in
  // read-only windowed mode with navigation — a growing log's tail is one
  // click away instead of invisible; fs/write saves whole-file edits through
  // a temp file + rename so a failed write never truncates the original.

  function utf8ToBase64(text: string): string {
    const bytes = new TextEncoder().encode(text);
    let binary = "";
    const step = 0x8000;
    for (let offset = 0; offset < bytes.length; offset += step) {
      binary += String.fromCharCode(...bytes.subarray(offset, offset + step));
    }
    return btoa(binary);
  }

  function base64ToUtf8(base64: string): string {
    const binary = atob(base64);
    const bytes = new Uint8Array(binary.length);
    for (let index = 0; index < binary.length; index++) bytes[index] = binary.charCodeAt(index);
    return new TextDecoder("utf-8").decode(bytes);
  }

  function openEditor(path: string): void {
    const overlay = document.createElement("div");
    overlay.className = "editor-overlay";
    overlay.innerHTML = `
      <div class="editor-card">
        <div class="editor-head">
          <span class="editor-title"></span>
          <span class="editor-status"></span>
          <span class="spacer"></span>
          <button class="btn small" data-act="save">${t("editorSave")}</button>
          <button class="btn ghost small" data-act="reload">${t("editorReload")}</button>
          <button class="btn ghost small" data-act="close">${t("editorClose")}</button>
        </div>
        <div class="editor-nav" hidden>
          <button class="btn ghost small" data-w="start" title="${t("editorStart")}">⏮</button>
          <button class="btn ghost small" data-w="prev" title="${t("editorPrev")}">‹ ${t("editorPrev")}</button>
          <button class="btn ghost small" data-w="next" title="${t("editorNext")}">${t("editorNext")} ›</button>
          <button class="btn ghost small" data-w="end" title="${t("editorEnd")}">${t("editorEnd")} ⏭</button>
          <span class="editor-range"></span>
          <span class="spacer"></span>
          <button class="btn small" data-act="download-full">${icon("download", 13)}${t("editorDownload")}</button>
        </div>
        <textarea class="editor-body" spellcheck="false" wrap="off"></textarea>
      </div>
    `;
    const title = overlay.querySelector<HTMLElement>(".editor-title")!;
    const status = overlay.querySelector<HTMLElement>(".editor-status")!;
    const nav = overlay.querySelector<HTMLElement>(".editor-nav")!;
    const range = overlay.querySelector<HTMLElement>(".editor-range")!;
    const area = overlay.querySelector<HTMLTextAreaElement>(".editor-body")!;
    const saveButton = overlay.querySelector<HTMLButtonElement>('[data-act="save"]')!;
    const reloadButton = overlay.querySelector<HTMLButtonElement>('[data-act="reload"]')!;
    title.textContent = path;
    overlay.querySelector('[data-act="close"]')!.addEventListener("click", () => overlay.remove());
    overlay.addEventListener("pointerdown", (event) => {
      if (event.target === overlay) overlay.remove();
    });

    let totalSize = 0;
    let offset = 0;
    let step = 1; // last served window size, updated per load
    let busy = false;
    const setBusy = (value: boolean): void => {
      busy = value;
      saveButton.disabled = value || area.readOnly;
      reloadButton.disabled = value;
      nav.querySelectorAll<HTMLButtonElement>("button").forEach((button) => {
        button.disabled = value;
      });
    };

    const loadWindow = async (next: number, jumpEnd = false): Promise<void> => {
      if (busy) return;
      busy = true;
      setBusy(true);
      status.textContent = t("connecting");
      try {
        const result = await api<{
          contentBase64?: string;
          binary?: boolean;
          size: number;
          offset: number;
          limit: number;
          windowed?: boolean;
        }>("fs/read", { shellId, path, offset: next }, 60000);
        if (result.binary) {
          area.value = "";
          area.readOnly = true;
          nav.hidden = true;
          saveButton.hidden = true;
          status.textContent = t("editorBinary");
          return;
        }
        totalSize = result.size;
        offset = result.offset;
        step = Math.max(1, result.limit);
        // windowed is a property of the FILE (bigger than one window), derived
        // locally: the tail window reaches EOF but must stay read-only — an
        // editable partial view saved back would truncate the whole file.
        const windowed = totalSize > result.limit;
        area.value = result.contentBase64 ? base64ToUtf8(result.contentBase64) : "";
        // readOnly (not disabled): the text stays selectable and copyable
        area.readOnly = windowed;
        saveButton.hidden = windowed;
        nav.hidden = !windowed;
        if (windowed) {
          range.textContent = `${humanSize(offset)} – ${humanSize(offset + result.limit)} / ${humanSize(totalSize)}`;
          status.textContent = t("editorWindowed");
        } else {
          status.textContent = humanSize(totalSize);
        }
        area.scrollTop = jumpEnd ? area.scrollHeight : 0;
      } catch (error: any) {
        status.textContent = "";
        notify(`${t("editorLoadFail")}: ${error.message}`);
        overlay.remove();
        return;
      } finally {
        setBusy(false);
      }
    };

    nav.addEventListener("click", (event) => {
      const which = (event.target as HTMLElement).closest<HTMLElement>("[data-w]")?.dataset.w;
      if (!which) return;
      if (which === "start") void loadWindow(0);
      else if (which === "prev") void loadWindow(Math.max(0, offset - step));
      else if (which === "next") void loadWindow(offset + step);
      else if (which === "end") void loadWindow(Math.max(0, totalSize - step), true);
    });
    overlay.querySelector<HTMLButtonElement>('[data-act="download-full"]')!.addEventListener("click", () => {
      void downloadByName(path.split("/").pop() || path, path, totalSize);
    });

    const save = async (): Promise<void> => {
      if (area.readOnly) return;
      saveButton.disabled = true;
      status.textContent = t("connecting");
      try {
        await api("fs/write", { shellId, path, contentBase64: utf8ToBase64(area.value) }, 120000);
        status.textContent = t("editorSaved");
        void load();
      } catch (error: any) {
        status.textContent = "";
        notify(`${t("editorSaveFail")}: ${error.message}`);
      } finally {
        saveButton.disabled = false;
      }
    };
    saveButton.addEventListener("click", () => void save());
    reloadButton.addEventListener("click", () => void loadWindow(offset));
    area.addEventListener("keydown", (event) => {
      if ((event.ctrlKey || event.metaKey) && event.key.toLowerCase() === "s") {
        event.preventDefault();
        if (!area.readOnly) void save();
      }
    });
    document.querySelector("#app")!.append(overlay);
    void loadWindow(0);
  }

  // ---- log follow viewer (tail -f) ----
  // One overlay, one xterm fed by the tail/<id> binary channel. An xterm
  // (not a textarea) gets the coloring rules, Ctrl+F search and huge
  // scrollback for free. Scroll lock: writing new output pins the view only
  // while the user is AT the bottom; scrolling up pauses following and shows
  // a "new output" pill, jumping back down resumes.
  function openLogView(path: string): void {
    const overlay = document.createElement("div");
    overlay.className = "editor-overlay";
    overlay.innerHTML = `
      <div class="editor-card log-card">
        <div class="editor-head">
          <span class="editor-title"></span>
          <span class="editor-status"></span>
          <span class="spacer"></span>
          <button class="btn small log-jump" data-act="jump" hidden>↓ 0</button>
          <button class="btn ghost small" data-act="close">${t("editorClose")}</button>
        </div>
        <div class="log-body"></div>
      </div>
    `;
    const title = overlay.querySelector<HTMLElement>(".editor-title")!;
    const status = overlay.querySelector<HTMLElement>(".editor-status")!;
    const body = overlay.querySelector<HTMLElement>(".log-body")!;
    const jump = overlay.querySelector<HTMLButtonElement>('[data-act="jump"]')!;
    title.textContent = path;

    const highlighter = new Highlighter({ enabled: true });
    const term = createTerminal(body);

    let detachChannel: (() => void) | undefined;
    let detachSettings: (() => void) | undefined;
    let detachEvents: (() => void) | undefined;
    let taskId = "";
    let closed = false;
    const close = (): void => {
      if (closed) return;
      closed = true;
      if (taskId) rpcNotify("tail/stop", { taskId });
      detachChannel?.();
      detachSettings?.();
      detachEvents?.();
      term.dispose();
      overlay.remove();
    };
    overlay.querySelector('[data-act="close"]')!.addEventListener("click", close);
    overlay.addEventListener("pointerdown", (event) => {
      if (event.target === overlay) close();
    });

    // live styling: apply current settings now and follow changes while open
    void loadStyleSettings().then((settings) => {
      highlighter.setEnabled(settings.highlight);
      highlighter.setRules(settings.rules);
      term.setScheme(settings.scheme);
    });
    detachSettings = onStyleSettingsChanged((settings) => {
      highlighter.setEnabled(settings.highlight);
      highlighter.setRules(settings.rules);
      term.setScheme(settings.scheme);
    });

    // scroll lock on the xterm viewport
    const viewport = body.querySelector<HTMLElement>(".xterm-viewport");
    let atBottom = true;
    let pendingLines = 0;
    const refreshPill = (): void => {
      jump.hidden = atBottom;
      jump.textContent = `↓ ${pendingLines}`;
    };
    if (viewport) {
      viewport.addEventListener("scroll", () => {
        const bottom =
          viewport.scrollTop + viewport.clientHeight >= viewport.scrollHeight - 8;
        if (bottom && !atBottom) pendingLines = 0;
        atBottom = bottom;
        refreshPill();
      });
    }
    jump.addEventListener("click", () => {
      atBottom = true;
      pendingLines = 0;
      term.terminal.scrollToBottom();
      refreshPill();
    });

    detachEvents = onSidecarEvent((message) => {
      if (message.method !== "tail.done" || String(message.params.taskId || "") !== taskId) return;
      status.textContent = `${t("followEnded")}: ${String(message.params.reason || "")}`;
    });

    document.querySelector("#app")!.append(overlay);
    term.fit();
    const fitObserver = new ResizeObserver(() => term.fit());
    fitObserver.observe(body);

    status.textContent = t("connecting");
    void (async () => {
      try {
        const start = await api<{ taskId: string }>("tail/start", { shellId, path, lines: 200 }, 30000);
        if (closed) return;
        taskId = start.taskId;
        // subscribe BEFORE ready: the follower emits nothing until tail/ready,
        // so no byte can be dropped by the demux
        detachChannel = onBinaryChannel(`tail/${taskId}`, (data) => {
          const styled = highlighter.process(data);
          term.write(styled);
          if (atBottom) {
            term.terminal.scrollToBottom();
          } else {
            for (const byte of styled) pendingLines += byte === 0x0a ? 1 : 0;
            refreshPill();
          }
        });
        rpcNotify("tail/ready", { taskId });
        status.textContent = t("followLive");
      } catch (error: any) {
        notify(`${t("followLog")}: ${error.message}`);
        close();
      }
    })();
  }

  return { root, followCwd };
}
