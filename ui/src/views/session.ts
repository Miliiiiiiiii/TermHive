// Session view: terminal(s) plus the optional file panel, opened from a DBX
// saved connection. Sessions live as internal tabs; tabs can be grouped —
// a group is one labeled unit in the tab strip whose members are always
// shown together as panes (up to PANE_CAP). Panes are stable slots inside
// the active selection: clicking a pane only moves focus.
import { bridge, hasFloating } from "../host";
import { icon } from "../icon";
import { t } from "../i18n";
import { notify } from "../notify";
import { createTerminal, type TerminalHandle } from "../term";
import { createFilePanel, type FilePanel } from "../filepanel";
import { Highlighter } from "../highlight";
import { createZmodemSentry } from "../zmodem";
import {
  defaultStyleSettings,
  loadStyleSettings,
  onStyleSettingsChanged,
  saveStyleSettings,
  type StyleSettings,
} from "../styleSettings";
import {
  api,
  ensureConnection,
  MINI_ID,
  newShellForConnection,
  onBinaryChannel,
  onSidecarEvent,
  sendBinary,
} from "../rpc";

export interface SessionContext {
  shellId?: string;
  connectionId?: string;
  name?: string;
  sub?: string;
  openFiles?: boolean;
}

export interface ConnectionInfo {
  connectionId: string;
  shellId: string;
  kind: string;
  name: string;
  host: string;
  username: string;
}

const PANE_CAP = 4;
const DRAG_TYPE = "application/x-dbx-tab";

interface ShellTab {
  shellId: string;
  // stable title: allocated once at creation (monotonic), renameable;
  // never recomputed from position so closing a tab never renames others
  title: string;
  tooltip?: string;
  // cross-server tabs carry their own connection (same-server tabs use the
  // view's connectionId)
  connectionId?: string;
  sub?: string;
  // membership in a tab group; group members always render as panes together
  groupId?: string;
  holder?: HTMLElement;
  headTitle?: HTMLElement;
  headBadge?: HTMLElement;
  termBox?: HTMLElement;
  term?: TerminalHandle;
  highlighter?: Highlighter;
  offBinary?: () => void;
  // rz/sz stream fork: sees the tab's output before the highlighter
  zmodemSentry?: { consume(data: Uint8Array): void; dispose(): void };
  attached?: boolean;
  filePanel?: FilePanel;
  // last cwd the shell reported via OSC 7 (fed to the panel when it opens)
  terminalCwd?: string;
  // fresh marks shells opened by this view: after first attach their
  // scrollback is trimmed once (server resize-repaints leak stray lines
  // into it); userTyped guards against trimming anything the user produced
  fresh?: boolean;
  userTyped?: boolean;
  // last dims actually sent to the PTY: every session/resize makes Windows
  // OpenSSH re-emit the whole screen, so no-op resizes must never go out
  sentCols?: number;
  sentRows?: number;
}

interface TabGroup {
  id: string;
  name: string;
}

export async function mountSession(context: SessionContext): Promise<() => void> {
  const root = document.createElement("div");
  root.className = "session";
  root.innerHTML = `
    <header class="bar">
      <span class="dot" id="status-dot"></span>
      <div class="shell-tabs" id="shell-tabs"></div>
      <button class="icon-btn" id="btn-new" title="${t("newTabSame")}">${icon("plus", 15)}</button>
      <span class="spacer"></span>
      <button class="icon-btn" id="btn-settings" title="${t("openSettings")}">${icon("settings", 15)}</button>
      <button class="icon-btn" id="btn-snips" title="${t("snippetsTitle")}">${icon("zap", 15)}</button>
      <button class="icon-btn" id="btn-broadcast" title="${t("broadcastTitle")}">${icon("signal", 15)}</button>
      <button class="icon-btn" id="btn-monitor" title="${t("monitorTitle")}">${icon("activity", 15)}</button>
      <button class="icon-btn" id="btn-server" title="${t("addServer")}">${icon("server", 15)}</button>
      <button class="icon-btn" id="btn-split" hidden>${icon("columns", 16)}</button>
      <button class="icon-btn" id="btn-files" title="${t("toggleFiles")}">${icon("files", 16)}</button>
      <button class="icon-btn" id="btn-pop" hidden title="${t("popOut")}">${icon("external", 16)}</button>
    </header>
    <div class="restore-bar" id="restore-bar" hidden>
      <span class="restore-text" id="restore-text"></span>
      <button class="btn" id="btn-restore">${icon("refresh", 12)}${t("restoreBtn")}</button>
      <button class="icon-btn" id="btn-restore-dismiss">${icon("close", 12)}</button>
    </div>
    <div class="session-body">
      <div class="term-wrap" id="term">
        <div class="panes" id="panes"></div>
      </div>
      <div class="splitter" id="splitter" hidden></div>
      <div class="files-wrap" id="files" hidden></div>
    </div>
    <div class="server-pop" id="server-pop" hidden></div>
    <div class="server-pop snips-pop" id="snips-pop" hidden></div>
    <div class="server-pop monitor-pop" id="monitor-pop" hidden></div>
    <div class="ctx-menu" id="tab-menu" hidden></div>
    <div class="overlay" id="overlay" hidden>
      <div class="overlay-card">
        <span class="overlay-icon">${icon("power", 20)}</span>
        <h3 id="overlay-title"></h3>
        <p id="overlay-reason"></p>
        <div class="overlay-actions">
          <button class="btn" id="btn-reconnect">${icon("refresh", 13)}${t("reconnect")}</button>
        </div>
      </div>
    </div>
  `;
  document.querySelector("#app")!.append(root);

  const statusDot = root.querySelector<HTMLElement>("#status-dot")!;
  const tabsBox = root.querySelector<HTMLElement>("#shell-tabs")!;
  const overlay = root.querySelector<HTMLElement>("#overlay")!;
  const overlayTitle = root.querySelector<HTMLElement>("#overlay-title")!;
  const overlayReason = root.querySelector<HTMLElement>("#overlay-reason")!;
  const panesBox = root.querySelector<HTMLElement>("#panes")!;
  const termWrap = root.querySelector<HTMLElement>("#term")!;
  const filesWrap = root.querySelector<HTMLElement>("#files")!;
  const splitter = root.querySelector<HTMLElement>("#splitter")!;
  const filesButton = root.querySelector<HTMLElement>("#btn-files")!;
  const newButton = root.querySelector<HTMLElement>("#btn-new")!;
  const popButton = root.querySelector<HTMLElement>("#btn-pop")!;
  const splitButton = root.querySelector<HTMLElement>("#btn-split")!;
  const serverButton = root.querySelector<HTMLElement>("#btn-server")!;
  const serverPop = root.querySelector<HTMLElement>("#server-pop")!;
  const snipsButton = root.querySelector<HTMLElement>("#btn-snips")!;
  const snipsPop = root.querySelector<HTMLElement>("#snips-pop")!;
  const broadcastButton = root.querySelector<HTMLElement>("#btn-broadcast")!;
  const monitorButton = root.querySelector<HTMLElement>("#btn-monitor")!;
  const monitorPop = root.querySelector<HTMLElement>("#monitor-pop")!;
  const tabMenu = root.querySelector<HTMLElement>("#tab-menu")!;

  let connectionId = context.connectionId ?? "";
  let sessionName = context.name ?? "";
  let sessionSub = context.sub ?? "";
  let closedNotified = false;
  let titleCounter = 0;
  let groupCounter = 0;

  const tabs: ShellTab[] = [];
  const groups = new Map<string, TabGroup>();

  // Per-connection health for the tab dots: the host's green list dot never
  // notices a dropped link (platform limitation), but the reconnect events
  // give us the truth. Keyed by connectionId; absent = healthy.
  const connStates = new Map<string, "reconnecting" | "failed">();
  function connStateOf(tab: ShellTab): "reconnecting" | "failed" | undefined {
    return connStates.get(tab.connectionId || connectionId);
  }
  function refreshConnDots(): void {
    renderTabs();
    for (const tab of tabs) syncPaneHead(tab);
  }

  // panes[i] is the tab index visible in pane slot i. Derived from the
  // focused tab's group (all members) or the focused tab alone; focusedPane
  // picks which slot has focus.
  let panes: number[] = [];
  let focusedPane = 0;

  function activeIndex(): number {
    return panes[focusedPane] ?? -1;
  }
  function currentShellId(): string {
    return tabs[activeIndex()]?.shellId ?? "";
  }
  function visibleTabs(): ShellTab[] {
    return panes.map((slot) => tabs[slot]).filter(Boolean);
  }
  function memberIndices(gid: string): number[] {
    const out: number[] = [];
    tabs.forEach((tab, index) => {
      if (tab.groupId === gid) out.push(index);
    });
    return out;
  }
  function currentGroupId(): string | undefined {
    return tabs[panes[0]]?.groupId;
  }

  const encoder = new TextEncoder();
  const attachFailed = new Set<string>();

  // ---- broadcast input (sync typing across the current group's panes) ----
  let broadcast = false;
  broadcastButton.addEventListener("click", () => {
    broadcast = !broadcast;
    broadcastButton.classList.toggle("active", broadcast);
    if (broadcast && panes.length < 2) notify(t("broadcastSolo"), "info");
    else notify(broadcast ? t("broadcastOn") : t("broadcastOff"), "info");
  });

  // ---- plugin-wide style settings (config lives on the settings page) ----
  const styleState: StyleSettings = defaultStyleSettings();
  function applyHighlightSettings(): void {
    for (const tab of tabs) {
      tab.highlighter?.setEnabled(styleState.highlight);
      tab.highlighter?.setRules(styleState.rules);
    }
  }
  function applySchemeAll(): void {
    for (const tab of tabs) tab.term?.setScheme(styleState.scheme);
  }
  function applyTerminalStyle(): void {
    for (const tab of tabs) {
      tab.term?.applyStyle({
        fontFamily: styleState.fontFamily,
        fontSize: styleState.fontSize,
        scrollback: styleState.scrollback,
      });
    }
  }
  function syncStyle(next: StyleSettings): void {
    Object.assign(styleState, next);
    applySchemeAll();
    applyTerminalStyle();
    applyHighlightSettings();
  }
  void loadStyleSettings().then(syncStyle);
  const detachStyleEvent = onStyleSettingsChanged(syncStyle);

  // ---- workspace persistence (one-click restore of the last layout) ----
  // The structure (which connections × how many sessions × which groups) is
  // snapshotted on every structural change; contents are not. Until the user
  // has decided on the restore banner nothing is saved, so merely opening a
  // workbench never clobbers the layout the banner is offering.
  const restoreBar = root.querySelector<HTMLElement>("#restore-bar")!;
  const restoreText = root.querySelector<HTMLElement>("#restore-text")!;
  let restoreDecided = false;
  let workspaceSaveTimer = 0;
  let lastWorkspaceJSON = "";

  interface SavedTab { connectionId?: string; title?: string }
  interface SavedGroup { name?: string; members?: number[] }

  function snapshotWorkspace(): { v: number; savedAt: number; tabs: SavedTab[]; groups: SavedGroup[] } {
    return {
      v: 1,
      savedAt: Date.now(),
      tabs: tabs.map((tab) => ({ connectionId: tab.connectionId || connectionId, title: tab.title })),
      groups: [...groups.values()].map((group) => ({ name: group.name, members: memberIndices(group.id) })),
    };
  }

  function scheduleSaveWorkspace(): void {
    if (!restoreDecided) return;
    window.clearTimeout(workspaceSaveTimer);
    workspaceSaveTimer = window.setTimeout(() => {
      const snapshot = snapshotWorkspace();
      const json = JSON.stringify(snapshot);
      if (json === lastWorkspaceJSON) return;
      lastWorkspaceJSON = json;
      void api("workspace/put", { workspace: snapshot }, 15000).catch(() => undefined);
    }, 800);
  }

  // restoreWorkspace rebuilds the saved layout: one new shell per saved tab
  // (titles preserved), then the groups on top. A server that can no longer
  // be dialed is skipped; its group membership drops out via the index map.
  async function restoreWorkspace(savedTabs: SavedTab[], savedGroups: SavedGroup[]): Promise<void> {
    const failed: number[] = [];
    // saved index → live index (0 maps to the already-open first tab when its
    // connection matches, which is the common case)
    const remap = new Map<number, number>();
    if (savedTabs[0] && (!savedTabs[0].connectionId || savedTabs[0].connectionId === (tabs[0]?.connectionId || connectionId))) {
      remap.set(0, 0);
      if (savedTabs[0].title) tabs[0].title = savedTabs[0].title;
    }
    for (let index = 0; index < savedTabs.length; index++) {
      if (remap.has(index)) continue;
      const target = savedTabs[index].connectionId || connectionId;
      try {
        const created = await newShellForConnection(
          target,
          tabs[activeIndex()]?.term?.cols() ?? 80,
          tabs[activeIndex()]?.term?.rows() ?? 24,
        );
        tabs.push({
          shellId: created.shellId,
          title: savedTabs[index].title || nextTitle(),
          connectionId: target === connectionId ? connectionId : target,
          sub: created.host,
          fresh: true,
        });
        remap.set(index, tabs.length - 1);
      } catch {
        failed.push(index);
      }
    }
    for (const group of savedGroups) {
      const members = (group.members || []).map((entry) => remap.get(entry)).filter((entry) => entry !== undefined) as number[];
      if (members.length < 2) continue;
      createGroup(members[0], members[1]);
      const gid = tabs[members[0]]?.groupId;
      if (gid) {
        const live = groups.get(gid);
        if (live && group.name) live.name = group.name;
        for (const entry of members.slice(2)) addToGroup(entry, gid);
      }
    }
    focusTab(0);
    if (failed.length > 0) notify(t("restoreFailedSome"));
    else notify(t("restoreDone"), "info");
    renderTabs();
    syncPaneHead(tabs[0]!);
  }

  function offerRestore(): void {
    restoreDecided = true;
    scheduleSaveWorkspace();
    void api<{ workspace: { tabs?: SavedTab[]; groups?: SavedGroup[] } }>("workspace/get", {}, 15000)
      .then((result) => {
        const savedTabs = Array.isArray(result.workspace?.tabs) ? result.workspace.tabs : [];
        const savedGroups = Array.isArray(result.workspace?.groups) ? result.workspace.groups : [];
        if (savedTabs.length <= 1 && savedGroups.length === 0) return;
        // already-live sessions the backend knew about (workbench tab reused
        // after a host restart) make restoration redundant
        if (tabs.length >= savedTabs.length) return;
        restoreText.textContent =
          t("restoreBarText").replace("{n}", String(savedTabs.length)) +
          (savedGroups.length > 0 ? t("restoreBarGroups").replace("{m}", String(savedGroups.length)) : "");
        restoreBar.hidden = false;
        restoreDecided = false; // freeze saving until the banner is answered
      })
      .catch(() => undefined);
  }

  root.querySelector<HTMLElement>("#btn-restore")!.addEventListener("click", () => {
    restoreBar.hidden = true;
    restoreDecided = true;
    void api<{ workspace: { tabs?: SavedTab[]; groups?: SavedGroup[] } }>("workspace/get", {}, 15000)
      .then((result) => {
        const savedTabs = Array.isArray(result.workspace?.tabs) ? result.workspace.tabs : [];
        const savedGroups = Array.isArray(result.workspace?.groups) ? result.workspace.groups : [];
        return restoreWorkspace(savedTabs, savedGroups);
      })
      .catch(() => undefined);
  });
  root.querySelector<HTMLElement>("#btn-restore-dismiss")!.addEventListener("click", () => {
    restoreBar.hidden = true;
    restoreDecided = true;
    scheduleSaveWorkspace();
  });

  const detachEvent = onSidecarEvent((message) => {
    if (message.method === "shell.closed") {
      const reason = String(message.params.reason || "");
      const index = tabs.findIndex((tab) => tab.shellId === message.params.shellId);
      if (index === -1) return;
      // The dead shell is a stale id from this tab's perspective: mark it so
      // the next connect() rebinds this tab to a fresh shell instead of
      // stacking an extra one.
      attachFailed.add(String(message.params.shellId || ""));
      if (index === activeIndex()) {
        showClosed(reason);
      } else {
        removeTabAt(index);
      }
      return;
    }
    if (message.method === "connection.dead") {
      // transport gone (keepalive verdict). With auto-reconnect on, a
      // reconnecting event follows almost immediately; with it off this is
      // the terminal state, so the dots show it directly.
      connStates.set(String(message.params.connectionId || ""), styleState.autoReconnect ? "reconnecting" : "failed");
      refreshConnDots();
      return;
    }
    if (message.method === "connection.reconnecting") {
      connStates.set(String(message.params.connectionId || ""), "reconnecting");
      refreshConnDots();
      if (String(message.params.connectionId || "") !== connectionId || !closedNotified) return;
      const attempt = Number(message.params.attempt || 0);
      overlayTitle.textContent = t("reconnecting");
      overlayReason.textContent = `${t("reconnectingWait")} · ${attempt}`;
      statusDot.className = "dot busy";
      overlay.hidden = false;
      return;
    }
    if (message.method === "connection.reconnected") {
      connStates.delete(String(message.params.connectionId || ""));
      refreshConnDots();
      if (String(message.params.connectionId || "") !== connectionId || !closedNotified) return;
      void connect();
      return;
    }
    if (message.method === "connection.failed") {
      connStates.set(String(message.params.connectionId || ""), "failed");
      refreshConnDots();
      if (String(message.params.connectionId || "") !== connectionId || !closedNotified) return;
      statusDot.className = "dot dead";
      overlayTitle.textContent = t("disconnected");
      overlayReason.textContent = `${t("reconnectFailed")}: ${String(message.params.reason || "")}`;
      overlay.hidden = false;
      return;
    }
  });

  function nextTitle(): string {
    titleCounter += 1;
    return `${t("sessionTab")} ${titleCounter}`;
  }

  // ---- per-tab terminal lifecycle ----

  // ensureTerm lazily builds the tab's own pane (header + xterm box) and
  // subscribes it to its binary channel for the tab's whole lifetime:
  // switching tabs only shows and hides DOM, so no reset/replay dance and no
  // cross-tab bleed.
  function ensureTerm(tab: ShellTab): void {
    if (tab.term) return;
    tab.holder = document.createElement("div");
    tab.holder.className = "term-holder";
    const head = document.createElement("div");
    head.className = "pane-head";
    head.innerHTML = `<span class="pane-head-icon">${icon("terminal", 12)}</span><span class="conn-dot"></span><span class="pane-head-title" title="${t("renameHint")}"></span><span class="pane-head-badge"></span><span class="pane-collapse" title="${t("leaveGroup")}">${icon("close", 11)}</span>`;
    tab.headTitle = head.querySelector<HTMLElement>(".pane-head-title")!;
    tab.headBadge = head.querySelector<HTMLElement>(".pane-head-badge")!;
    tab.termBox = document.createElement("div");
    tab.termBox.className = "pane-term";
    tab.holder.append(head, tab.termBox);

    tab.term = createTerminal(tab.termBox, styleState.scheme, {
      // the shell's OSC 7 cwd reports drive the file panel's follow mode;
      // cached on the tab so a panel opened later starts in the right place
      onCwd: (path) => {
        tab.terminalCwd = path;
        tab.filePanel?.followCwd(path);
      },
      style: {
        fontFamily: styleState.fontFamily,
        fontSize: styleState.fontSize,
        scrollback: styleState.scrollback,
      },
    });
    tab.term.onData((data) => {
      tab.userTyped = true;
      // read shellId at send time: rebinds after a redial rename the channel
      sendBinary(`term/${tab.shellId}`, encoder.encode(data));
      // broadcast input: every keystroke of the focused pane is mirrored to
      // the other panes of the current group (manage N servers in lockstep)
      if (broadcast) {
        for (const peer of visibleTabs()) {
          if (peer !== tab) sendBinary(`term/${peer.shellId}`, encoder.encode(data));
        }
      }
    });
    tab.highlighter = new Highlighter({ enabled: styleState.highlight, rules: styleState.rules });
    // the sentry forks the output stream: ZMODEM frames (rz/sz typed on the
    // server) become transfers, everything else flows through the normal
    // path — and it sits BEFORE the highlighter on purpose (protocol frames
    // must never be re-encoded)
    tab.zmodemSentry = createZmodemSentry({
      writeTerm: (data) => tab.term!.write(tab.highlighter!.process(data)),
      sendToPty: (data) => sendBinary(`term/${tab.shellId}`, data),
      notify: (text, kind) => notify(text, kind),
      diag: (line) => {
        void api("diag/log", { line: `zmodem: ${line}` }, 5000).catch(() => undefined);
      },
    });
    tab.offBinary = onBinaryChannel(`term/${tab.shellId}`, (data) => {
      tab.zmodemSentry!.consume(data);
    });

    // clicking a pane only moves focus — terminals stay where they are
    tab.holder.addEventListener("pointerdown", () => {
      const slot = panes.indexOf(tabs.indexOf(tab));
      if (slot !== -1) setFocusedPane(slot);
    });
    // drop target: a tab dropped onto this pane joins the pane's group at
    // that slot (or forms a new group with a lone pane)
    tab.holder.addEventListener("dragover", (event) => {
      if (!event.dataTransfer?.types.includes(DRAG_TYPE)) return;
      event.preventDefault();
      event.stopPropagation();
      tab.holder!.classList.add("drop-hot");
    });
    tab.holder.addEventListener("dragleave", () => tab.holder!.classList.remove("drop-hot"));
    tab.holder.addEventListener("drop", (event) => {
      event.stopPropagation();
      tab.holder!.classList.remove("drop-hot");
      const raw = event.dataTransfer?.getData(DRAG_TYPE);
      if (!raw) return;
      event.preventDefault();
      const slot = panes.indexOf(tabs.indexOf(tab));
      if (slot !== -1) putTabInPane(Number(raw), slot);
    });
    tab.headTitle.addEventListener("dblclick", (event) => {
      event.stopPropagation();
      startRename(tab, tab.headTitle!);
    });
    // the pane × removes this session from its group (the session itself
    // stays alive in the tab strip)
    head.querySelector<HTMLElement>(".pane-collapse")!.addEventListener("click", (event) => {
      event.stopPropagation();
      const index = tabs.indexOf(tab);
      if (index !== -1) leaveGroup(index);
    });
    syncPaneHead(tab);
  }

  // syncPaneHead mirrors the tab identity into its pane header; the badge
  // marks sessions belonging to another server than this workbench tab.
  function syncPaneHead(tab: ShellTab): void {
    if (!tab.headTitle) return;
    tab.headTitle.textContent = tab.title;
    const foreign = tab.connectionId && tab.connectionId !== connectionId;
    if (tab.headBadge) {
      tab.headBadge.textContent = foreign ? tab.sub || "" : "";
      tab.headBadge.hidden = !foreign;
    }
    const state = connStateOf(tab);
    tab.holder?.classList.toggle("conn-busy", state === "reconnecting");
    tab.holder?.classList.toggle("conn-dead", state === "failed");
    const dot = tab.holder?.querySelector<HTMLElement>(".pane-head .conn-dot");
    if (dot) dot.title = state ? (state === "reconnecting" ? t("reconnecting") : t("reconnectFailed")) : "";
  }

  function disposeTab(tab: ShellTab): void {
    tab.offBinary?.();
    tab.offBinary = undefined;
    tab.zmodemSentry?.dispose();
    tab.zmodemSentry = undefined;
    tab.term?.dispose();
    tab.term = undefined;
    tab.holder?.remove();
    tab.holder = undefined;
    tab.headTitle = undefined;
    tab.headBadge = undefined;
    tab.termBox = undefined;
    tab.filePanel = undefined;
  }

  // bindShell re-points a tab at a fresh shell (redial after the old one
  // died): fresh channel, cleared screen, dropped file panel.
  function bindShell(tab: ShellTab, shellId: string): void {
    if (tab.shellId === shellId) return;
    tab.offBinary?.();
    tab.shellId = shellId;
    tab.attached = false;
    tab.filePanel = undefined;
    tab.terminalCwd = undefined;
    // fresh PTY: its size is whatever the redial requested, not our cache
    tab.sentCols = undefined;
    tab.sentRows = undefined;
    if (tab.term) {
      tab.term.reset();
      // fresh PTY, fresh stream state: a new highlighter (the old one's
      // decoder carries cross-chunk state from the dead shell) and a new
      // zmodem sentry on the renamed channel
      tab.highlighter = new Highlighter({ enabled: styleState.highlight, rules: styleState.rules });
      tab.zmodemSentry?.dispose();
      tab.zmodemSentry = createZmodemSentry({
        writeTerm: (data) => tab.term!.write(tab.highlighter!.process(data)),
        sendToPty: (data) => sendBinary(`term/${tab.shellId}`, data),
        notify: (text, kind) => notify(text, kind),
        diag: (line) => {
          void api("diag/log", { line: `zmodem: ${line}` }, 5000).catch(() => undefined);
        },
      });
      tab.offBinary = onBinaryChannel(`term/${shellId}`, (data) => tab.zmodemSentry!.consume(data));
    }
    renderTabs();
  }

  // activateTerminal performs the one-time attach (backend replay catches
  // the tab up on history) and keeps the PTY sized to the pane.
  async function activateTerminal(tab: ShellTab): Promise<void> {
    ensureTerm(tab);
    const firstAttach = !tab.attached;
    if (!tab.attached) {
      tab.attached = true;
      const ok = await api<{ closed: boolean }>("session/attach", { shellId: tab.shellId }, 30000).then(
        () => {
          attachFailed.delete(tab.shellId);
          return true;
        },
        () => {
          attachFailed.add(tab.shellId);
          return false;
        },
      );
      if (!ok) {
        const owner = tab.connectionId || connectionId;
        if (!owner) {
          showClosed("session is gone");
          return;
        }
        // the shell behind this tab died before we attached (e.g. the tab
        // predates a reconnect): redial and rebind in place
        try {
          const next = await newShellForConnection(owner, tab.term?.cols() ?? 80, tab.term?.rows() ?? 24);
          bindShell(tab, next.shellId);
          attachFailed.delete(next.shellId);
          await api("session/attach", { shellId: tab.shellId }, 30000).catch(() => undefined);
        } catch (error: any) {
          showClosed(error.message);
          return;
        }
      }
    }
    requestAnimationFrame(() => {
      tab.term?.fit();
      tab.term?.terminal.scrollToBottom();
      // resize must be sent with the FITTED dims: sending the pre-fit
      // defaults makes the server repaint the screen at the wrong size and
      // the repainting frame leaks rows into scrollback
      sendResizeIfChanged(tab);
    });
    if (firstAttach && tab.fresh) {
      // brand-new shell: server resize-repaint frames leak rows into
      // scrollback, and they can land LATE (font loading re-fits the
      // terminal after seconds). Keep trimming the scrollback on a watch
      // loop while the user has not typed anything yet — the first
      // keystroke stops it permanently; every trim is logged for forensics.
      const startedAt = Date.now();
      const watcher = window.setInterval(() => {
        if (!tab.term || tab.userTyped || Date.now() - startedAt > 12000) {
          window.clearInterval(watcher);
          return;
        }
        const baseY = tab.term.terminal.buffer.active.baseY;
        if (baseY > 0) {
          tab.term.terminal.clear();
          void api(
            "diag/log",
            { line: `fresh shell trimmed baseY=${baseY} t=${Date.now() - startedAt}ms` },
            5000,
          ).catch(() => undefined);
        }
      }, 800);
    }
  }

  // sendResizeIfChanged forwards the pane's current dims to the PTY, but
  // only when they differ from the last sent pair: identical dims mean an
  // identical screen, yet Windows OpenSSH still re-emits the whole screen
  // for every window-change (repro: scripts/resize-repro.mjs) — each extra
  // frame pollutes scrollback with a duplicated banner.
  function sendResizeIfChanged(tab: ShellTab): void {
    if (!tab.term) return;
    const cols = tab.term.cols();
    const rows = tab.term.rows();
    if (tab.sentCols === cols && tab.sentRows === rows) return;
    tab.sentCols = cols;
    tab.sentRows = rows;
    void api("session/resize", { shellId: tab.shellId, cols, rows }).catch(() => undefined);
  }

  let resizeTimer = 0;
  const observer = new ResizeObserver(() => {
    for (const tab of visibleTabs()) tab.term?.fit();
    window.clearTimeout(resizeTimer);
    resizeTimer = window.setTimeout(() => {
      for (const tab of visibleTabs()) sendResizeIfChanged(tab);
    }, 150);
  });
  observer.observe(termWrap);

  // dropping a tab on the pane area's empty space appends it to the active
  // group (or pairs it with the focused tab)
  panesBox.addEventListener("dragover", (event) => {
    if (!event.dataTransfer?.types.includes(DRAG_TYPE)) return;
    event.preventDefault();
  });
  panesBox.addEventListener("drop", (event) => {
    const raw = event.dataTransfer?.getData(DRAG_TYPE);
    if (!raw) return;
    event.preventDefault();
    addTabAsPane(Number(raw));
  });

  // ---- groups ----

  // focusTab derives the whole visible selection from the focused tab: its
  // group's members in tab-strip order, or the tab alone.
  function focusTab(index: number): void {
    if (index < 0 || index >= tabs.length) index = Math.max(0, tabs.length - 1);
    const gid = tabs[index]?.groupId;
    panes = gid ? memberIndices(gid) : [index];
    focusedPane = Math.max(0, panes.indexOf(index));
    afterFocusChange();
  }

  function createGroup(anchor: number, other: number): void {
    const group: TabGroup = {
      id: `g${Date.now().toString(36)}${Math.floor(Math.random() * 1e4)}`,
      name: `${t("groupDefaultName")} ${++groupCounter}`,
    };
    groups.set(group.id, group);
    tabs[anchor].groupId = group.id;
    tabs[other].groupId = group.id;
    focusTab(anchor);
  }

  function addToGroup(index: number, gid: string): void {
    const group = groups.get(gid);
    if (!group) return;
    if (memberIndices(gid).length >= PANE_CAP) {
      notify(t("splitMore"));
      return;
    }
    tabs[index].groupId = gid;
    focusTab(index);
  }

  // dissolveGroup releases every member back to the strip (sessions stay).
  function dissolveGroup(gid: string): void {
    const members = memberIndices(gid);
    for (const index of members) tabs[index].groupId = undefined;
    groups.delete(gid);
    if (tabs[panes[0]]?.groupId !== gid && !members.includes(panes[0])) {
      // viewing something else: only the strip changes
      renderTabs();
      return;
    }
    focusTab(members[0] ?? activeIndex());
  }

  // leaveGroup pulls one tab out of its group; a group that falls to a
  // single member dissolves (a lone tab needs no group).
  function leaveGroup(index: number): void {
    const gid = tabs[index]?.groupId;
    if (!gid) {
      focusTab(index);
      return;
    }
    tabs[index].groupId = undefined;
    const rest = memberIndices(gid);
    if (rest.length <= 1) {
      if (rest.length === 1) tabs[rest[0]].groupId = undefined;
      groups.delete(gid);
    }
    focusTab(index);
  }

  // putTabInPane is the pane-slot drop: the dropped tab joins the slot's
  // group at that position; dropping onto a lone pane pairs the two into a
  // new group. A full group swaps the slot's occupant out instead.
  function putTabInPane(index: number, slot: number): void {
    if (slot < 0 || slot >= panes.length) return;
    const gid = currentGroupId();
    if (!gid) {
      const current = panes[0];
      if (current === index) return;
      createGroup(slot === 0 ? index : current, slot === 0 ? current : index);
      return;
    }
    if (tabs[index].groupId === gid && memberIndices(gid).length === 1) return;
    const order = panes.filter((entry) => entry !== index);
    const oldSlot = panes.indexOf(index);
    const adjusted = oldSlot !== -1 && oldSlot < slot ? slot - 1 : slot;
    if (!order.includes(index) && order.length >= PANE_CAP) {
      // group is full: the occupant of this slot returns to the strip
      const occupant = order[adjusted] ?? order[order.length - 1];
      if (occupant !== undefined) {
        tabs[occupant].groupId = undefined;
        const rest = memberIndices(gid);
        if (rest.length <= 1) {
          if (rest.length === 1) tabs[rest[0]].groupId = undefined;
          groups.delete(gid);
        }
      }
    }
    const insertAt = Math.min(adjusted, order.length);
    order.splice(insertAt, 0, index);
    panes = order;
    for (const entry of order) tabs[entry].groupId = gid;
    focusedPane = Math.max(0, panes.indexOf(index));
    afterFocusChange();
  }

  // addTabAsPane appends a tab to the active group (or pairs it with the
  // focused tab when nothing is grouped).
  function addTabAsPane(index: number): void {
    const gid = currentGroupId();
    if (!gid) {
      const current = panes[0];
      if (current === index) return;
      createGroup(current, index);
      return;
    }
    addToGroup(index, gid);
  }

  // ---- tabs strip ----

  // chipNameSpans lets the context menu reach the right chip's name span
  // after renderTabs (chips live at different depths once grouped)
  const chipNameSpans = new Map<number, HTMLElement>();

  function buildChip(tab: ShellTab, index: number, focused: number): HTMLElement {
    // div, not button: the rename input must live inside (interactive
    // elements cannot nest inside <button> — the input would be dead)
    const chip = document.createElement("div");
    chip.className =
      "shell-tab" +
      (index === focused ? " active" : "") +
      (panes.includes(index) ? " peer" : "");
    const state = connStateOf(tab);
    if (state === "reconnecting") chip.classList.add("conn-busy");
    else if (state === "failed") chip.classList.add("conn-dead");
    chip.setAttribute("role", "tab");
    chip.innerHTML = `<span class="conn-dot" title="${state ? (state === "reconnecting" ? t("reconnecting") : t("reconnectFailed")) : ""}"></span><span class="shell-tab-name" title="${t("renameHint")}"></span><span class="shell-tab-close" title="${t("closeConn")}">${icon("close", 11)}</span>`;
    const nameSpan = chip.querySelector<HTMLElement>(".shell-tab-name")!;
    nameSpan.textContent = tab.title;
    chipNameSpans.set(index, nameSpan);
    chip.addEventListener("click", (event) => {
      if ((event.target as HTMLElement).closest(".shell-tab-close")) return;
      clickTab(index);
    });
    nameSpan.addEventListener("dblclick", (event) => {
      event.stopPropagation();
      startRename(tab, nameSpan);
    });
    // dragging: onto another tab = group with it, onto a pane = slot
    chip.draggable = true;
    chip.addEventListener("dragstart", (event) => {
      event.dataTransfer?.setData(DRAG_TYPE, String(index));
      if (event.dataTransfer) event.dataTransfer.effectAllowed = "move";
    });
    chip.addEventListener("dragover", (event) => {
      if (!event.dataTransfer?.types.includes(DRAG_TYPE)) return;
      event.preventDefault();
      event.stopPropagation();
      chip.classList.add("drop-hot");
    });
    chip.addEventListener("dragleave", () => chip.classList.remove("drop-hot"));
    chip.addEventListener("drop", (event) => {
      event.stopPropagation();
      chip.classList.remove("drop-hot");
      const raw = event.dataTransfer?.getData(DRAG_TYPE);
      if (!raw) return;
      event.preventDefault();
      groupWithTarget(Number(raw), index);
    });
    chip.addEventListener("contextmenu", (event) => {
      event.preventDefault();
      openTabMenu(index, event.clientX, event.clientY);
    });
    chip.querySelector<HTMLElement>(".shell-tab-close")!.addEventListener("click", () => void closeTab(index));
    return chip;
  }

  // groupWithTarget follows the anchor: the dragged tab joins whatever the
  // drop target belongs to (its group, or a fresh group of the two).
  function groupWithTarget(dragged: number, anchor: number): void {
    if (dragged === anchor || !tabs[dragged] || !tabs[anchor]) return;
    const gid = tabs[anchor].groupId;
    if (gid) {
      if (tabs[dragged].groupId === gid) return;
      addToGroup(dragged, gid);
      return;
    }
    // the anchor is a lone tab: the dragged one leaves any group and pairs
    // with it — the drop target is always what wins
    tabs[dragged].groupId = undefined;
    createGroup(anchor, dragged);
  }

  function startGroupRename(group: TabGroup, label: HTMLElement): void {
    const input = document.createElement("input");
    input.type = "text";
    input.value = group.name;
    input.maxLength = 16;
    input.className = "shell-tab-rename";
    label.replaceWith(input);
    input.focus();
    input.select();
    let done = false;
    const commit = (save: boolean): void => {
      if (done) return;
      done = true;
      const value = input.value.trim();
      if (save && value) group.name = value;
      renderTabs();
    };
    input.addEventListener("keydown", (event) => {
      event.stopPropagation();
      if (event.key === "Enter") commit(true);
      else if (event.key === "Escape") commit(false);
    });
    input.addEventListener("blur", () => commit(true));
    input.addEventListener("click", (event) => event.stopPropagation());
  }

  function renderTabs(): void {
    tabsBox.innerHTML = "";
    const focused = activeIndex();
    const seenGroups = new Set<string>();
    tabs.forEach((tab, index) => {
      const gid = tab.groupId;
      if (gid && !groups.has(gid)) tab.groupId = undefined;
      if (tab.groupId) {
        if (seenGroups.has(tab.groupId)) return;
        seenGroups.add(tab.groupId);
        const group = groups.get(tab.groupId)!;
        const cluster = document.createElement("div");
        cluster.className = "tab-group" + (panes.some((slot) => tabs[slot]?.groupId === group.id) ? " active" : "");
        const label = document.createElement("span");
        label.className = "tab-group-name";
        label.textContent = group.name;
        label.title = t("renameHint");
        label.addEventListener("dblclick", (event) => {
          event.stopPropagation();
          startGroupRename(group, label);
        });
        const members = document.createElement("div");
        members.className = "tab-group-tabs";
        // dropping a tab onto the cluster joins the group
        cluster.addEventListener("dragover", (event) => {
          if (!event.dataTransfer?.types.includes(DRAG_TYPE)) return;
          event.preventDefault();
          event.stopPropagation();
          cluster.classList.add("drop-hot");
        });
        cluster.addEventListener("dragleave", () => cluster.classList.remove("drop-hot"));
        cluster.addEventListener("drop", (event) => {
          event.stopPropagation();
          cluster.classList.remove("drop-hot");
          const raw = event.dataTransfer?.getData(DRAG_TYPE);
          if (!raw) return;
          event.preventDefault();
          addToGroup(Number(raw), group.id);
        });
        const dissolve = document.createElement("span");
        dissolve.className = "tab-group-close";
        dissolve.title = t("dissolveGroup");
        dissolve.innerHTML = icon("close", 11);
        dissolve.addEventListener("click", (event) => {
          event.stopPropagation();
          dissolveGroup(group.id);
        });
        tabs.forEach((memberTab, memberIndex) => {
          if (memberTab.groupId === group.id) members.append(buildChip(memberTab, memberIndex, focused));
        });
        cluster.append(label, members, dissolve);
        tabsBox.append(cluster);
        return;
      }
      tabsBox.append(buildChip(tab, index, focused));
    });
    newButton.hidden = tabs.length === 0;
    splitButton.hidden = tabs.length === 0;
    splitButton.title = panes.length > 1 ? t("splitMore") : t("toggleSplit");
    scheduleSaveWorkspace();
  }

  // dropping a tab on the strip's empty area pulls it out of its group
  tabsBox.addEventListener("dragover", (event) => {
    if (!event.dataTransfer?.types.includes(DRAG_TYPE)) return;
    event.preventDefault();
  });
  tabsBox.addEventListener("drop", (event) => {
    const raw = event.dataTransfer?.getData(DRAG_TYPE);
    if (!raw) return;
    const index = Number(raw);
    if (tabs[index]?.groupId) {
      event.preventDefault();
      leaveGroup(index);
    }
  });

  function startRename(tab: ShellTab, nameSpan: HTMLElement): void {
    const input = document.createElement("input");
    input.type = "text";
    input.value = tab.title;
    input.maxLength = 24;
    input.className = "shell-tab-rename";
    nameSpan.replaceWith(input);
    input.focus();
    input.select();
    let done = false;
    const commit = (save: boolean): void => {
      if (done) return;
      done = true;
      const value = input.value.trim();
      if (save && value) tab.title = value;
      renderTabs();
      syncPaneHead(tab);
    };
    input.addEventListener("keydown", (event) => {
      event.stopPropagation();
      if (event.key === "Enter") commit(true);
      else if (event.key === "Escape") commit(false);
    });
    input.addEventListener("blur", () => commit(true));
    input.addEventListener("click", (event) => event.stopPropagation());
  }

  // renderPanes lays the visible tabs into their slots; pane headers appear
  // as soon as more than one pane is shown.
  function renderPanes(): void {
    panesBox.innerHTML = "";
    panesBox.className = "panes" + (panes.length > 1 ? ` cols-${panes.length}` : "");
    for (const slot of panes) {
      const tab = tabs[slot];
      if (!tab) continue;
      ensureTerm(tab);
      panesBox.append(tab.holder!);
      void activateTerminal(tab);
    }
    applyPaneFocusClasses();
  }

  // pane focus highlight only — no DOM churn, so pane clicks never flicker
  function applyPaneFocusClasses(): void {
    panes.forEach((slot, index) => {
      tabs[slot]?.holder?.classList.toggle("focused", index === focusedPane);
    });
  }

  // clickTab: focus the tab if it is on screen (same selection), otherwise
  // switch the whole selection to its group. Panes never swap.
  function clickTab(index: number): void {
    const slot = panes.indexOf(index);
    if (slot !== -1) {
      setFocusedPane(slot);
      return;
    }
    focusTab(index);
  }

  function setFocusedPane(slot: number): void {
    if (slot < 0 || slot >= panes.length) slot = 0;
    if (focusedPane === slot) return;
    focusedPane = slot;
    refreshFocus();
  }

  function refreshFocus(): void {
    renderTabs();
    applyPaneFocusClasses();
    closedNotified = false;
    overlay.hidden = true;
    statusDot.className = "dot live";
    const tab = tabs[activeIndex()];
    if (tab?.term) {
      requestAnimationFrame(() => tab.term?.terminal.focus());
    }
    if (!filesWrap.hidden) attachPanel();
    setTitle();
  }

  function afterFocusChange(): void {
    closedNotified = false;
    overlay.hidden = true;
    statusDot.className = "dot live";
    renderTabs();
    renderPanes();
    if (!filesWrap.hidden) attachPanel();
    setTitle();
  }

  function removeTabAt(index: number): void {
    const wasFocused = index === activeIndex();
    // group bookkeeping first (single-member groups dissolve)
    const gid = tabs[index]?.groupId;
    if (gid) {
      const rest = memberIndices(gid).filter((entry) => entry !== index);
      if (rest.length <= 1) {
        if (rest.length === 1) tabs[rest[0]].groupId = undefined;
        groups.delete(gid);
      }
    }
    const fallback = wasFocused
      ? Math.min(index, tabs.length - 2)
      : Math.max(0, activeIndex() - (index < activeIndex() ? 1 : 0));
    disposeTab(tabs[index]);
    tabs.splice(index, 1);
    if (tabs.length === 0) return;
    focusTab(Math.min(fallback, tabs.length - 1));
  }

  async function closeTab(index: number): Promise<void> {
    const id = tabs[index]?.shellId;
    if (!id) return;
    await api("session/close", { shellId: id }).catch(() => undefined);
    if (tabs.length === 1) {
      bridge().closeWorkbench?.().catch(() => undefined);
      return;
    }
    removeTabAt(index);
  }

  // openTabMenu is the right-click menu on a tab chip: grouping plus the
  // per-tab actions.
  function openTabMenu(index: number, x: number, y: number): void {
    tabMenu.innerHTML = "";
    const item = (label: string, action: () => void, disabled = false) => {
      const entry = document.createElement("button");
      entry.className = "ctx-item" + (disabled ? " disabled" : "");
      entry.textContent = label;
      entry.disabled = disabled;
      entry.addEventListener("click", () => {
        tabMenu.hidden = true;
        action();
      });
      tabMenu.append(entry);
    };
    const gid = tabs[index]?.groupId;
    const viewGid = currentGroupId();
    if (gid) {
      item(t("leaveGroup"), () => leaveGroup(index));
      item(t("dissolveGroup"), () => dissolveGroup(gid));
    } else if (viewGid) {
      item(t("joinGroup"), () => addToGroup(index, viewGid), memberIndices(viewGid).length >= PANE_CAP);
    }
    item(t("showInNewPane"), () => addTabAsPane(index));
    item(t("rename"), () => {
      const nameSpan = chipNameSpans.get(index);
      if (nameSpan && tabs[index]) startRename(tabs[index], nameSpan);
    });
    item(t("closeConn"), () => void closeTab(index));
    tabMenu.hidden = false;
    const rect = tabMenu.getBoundingClientRect();
    tabMenu.style.left = `${Math.min(x, window.innerWidth - rect.width - 8)}px`;
    tabMenu.style.top = `${Math.min(y, window.innerHeight - rect.height - 8)}px`;
  }
  document.addEventListener("pointerdown", (event) => {
    if (!tabMenu.hidden && !tabMenu.contains(event.target as Node)) tabMenu.hidden = true;
  });

  function setTitle(): void {
    bridge().setTitle?.(sessionName || currentShellId()).catch(() => undefined);
  }

  function showClosed(reason: string): void {
    if (closedNotified) return;
    closedNotified = true;
    if (styleState.autoReconnect) {
      // The backend supervisor is (about to be) retrying; show that state
      // instead of a dead end — connection.reconnecting will refine the text.
      statusDot.className = "dot busy";
      overlayTitle.textContent = t("reconnecting");
      overlayReason.textContent = `${reason}\n${t("reconnectingWait")}`;
    } else {
      statusDot.className = "dot dead";
      overlayTitle.textContent = t("disconnected");
      overlayReason.textContent = reason;
    }
    overlay.hidden = false;
  }

  async function connect(): Promise<void> {
    closedNotified = false;
    statusDot.className = "dot busy";
    overlay.hidden = true;
    if (!connectionId) {
      showClosed("tab context has no connectionId");
      return;
    }
    try {
      const next = await ensureConnection(connectionId);
      if (!connectionId) connectionId = next.connectionId;
      if (!sessionName) sessionName = next.name;
      sessionSub = next.username && next.host ? `${next.username}@${next.host}` : next.host;
      const existing = tabs.findIndex((tab) => tab.shellId === next.shellId);
      if (existing !== -1) {
        clickTab(existing);
        return;
      }
      // The primary shell was redialed under a new id (host re-connect after
      // backend restart). Any dead tab holding a stale shellId migrates to
      // the fresh shell instead of accumulating unusable tabs.
      const stale = tabs.findIndex((tab) => attachFailed.has(tab.shellId));
      if (stale !== -1) {
        bindShell(tabs[stale], next.shellId);
        clickTab(stale);
        return;
      }
      tabs.push({ shellId: next.shellId, title: nextTitle(), connectionId, fresh: true });
      focusTab(tabs.length - 1);
    } catch (error: any) {
      showClosed(error.message);
    }
  }

  // ---- initial attach or reconnect ----
  if (context.shellId) {
    tabs.push({ shellId: context.shellId, title: nextTitle(), connectionId: connectionId || undefined });
    panes = [0];
    afterFocusChange();
    if (connectionId) {
      void ensureConnection(connectionId)
        .then((next) => {
          if (!sessionName) sessionName = next.name;
          sessionSub = next.username && next.host ? `${next.username}@${next.host}` : next.host;
          setTitle();
        })
        .catch(() => undefined);
    }
  } else if (connectionId) {
    await connect();
  } else {
    showClosed("no connection context");
  }
  // the first session is up: offer to bring back the rest of last time's
  // layout (no-op when nothing richer than one tab was ever saved)
  offerRestore();

  // ---- file panel ----
  // One panel instance per session tab: switching re-attaches the existing
  // DOM node, so the browsed directory and transfer queue survive.
  function attachPanel(): void {
    const tab = tabs[activeIndex()];
    if (!tab) return;
    filesWrap.innerHTML = "";
    if (!tab.filePanel) {
      // born in the terminal's directory when the shell already reported one
      // (OSC 7) — otherwise the home listing races the follow jump
      tab.filePanel = createFilePanel(tab.shellId, tab.terminalCwd, tab.connectionId || connectionId);
    }
    filesWrap.append(tab.filePanel.root);
    for (const visible of visibleTabs()) visible.term?.fit();
  }
  function mountFiles(): void {
    if (currentShellId()) attachPanel();
    filesWrap.hidden = false;
    splitter.hidden = false;
    filesButton.classList.add("active");
    for (const visible of visibleTabs()) visible.term?.fit();
  }
  function hideFiles(): void {
    filesWrap.hidden = true;
    splitter.hidden = true;
    filesButton.classList.remove("active");
    for (const visible of visibleTabs()) visible.term?.fit();
  }
  if (context.openFiles) mountFiles();

  filesButton.addEventListener("click", () => {
    if (filesWrap.hidden) mountFiles();
    else hideFiles();
  });

  // ---- splitter drag ----
  let dragging = false;
  splitter.addEventListener("pointerdown", (event) => {
    dragging = true;
    splitter.setPointerCapture(event.pointerId);
  });
  splitter.addEventListener("pointermove", (event) => {
    if (!dragging) return;
    const body = root.querySelector<HTMLElement>(".session-body")!;
    const ratio = Math.min(0.8, Math.max(0.2, 1 - event.clientY / body.getBoundingClientRect().height));
    filesWrap.style.flexBasis = `${ratio * 100}%`;
    for (const visible of visibleTabs()) visible.term?.fit();
  });
  splitter.addEventListener("pointerup", () => {
    for (const visible of visibleTabs()) visible.term?.fit();
  });

  // ---- toolbar ----
  // 新开会话 opens a standalone session tab; group it explicitly (drag one
  // tab onto another, or use the context menu) when you want panes.
  newButton.addEventListener("click", async () => {
    if (!connectionId) return;
    try {
      const created = await newShellForConnection(
        connectionId,
        tabs[activeIndex()]?.term?.cols() ?? 80,
        tabs[activeIndex()]?.term?.rows() ?? 24,
      );
      tabs.push({ shellId: created.shellId, title: nextTitle(), connectionId, fresh: true });
      focusTab(tabs.length - 1);
    } catch (error: any) {
      notify(error.message);
    }
  });

  // "分栏" adds one more pane to the focused tab's group, creating a group
  // of two when the focused tab is standalone.
  splitButton.addEventListener("click", async () => {
    const focused = tabs[activeIndex()];
    const gid = focused?.groupId;
    if (gid && memberIndices(gid).length >= PANE_CAP) {
      notify(t("splitMore"));
      return;
    }
    const owner = focused?.connectionId || connectionId;
    if (!owner) return;
    try {
      const created = await newShellForConnection(
        owner,
        focused?.term?.cols() ?? 80,
        focused?.term?.rows() ?? 24,
      );
      tabs.push({ shellId: created.shellId, title: nextTitle(), connectionId: owner, fresh: true });
      const fresh = tabs.length - 1;
      if (gid) addToGroup(fresh, gid);
      else createGroup(activeIndex(), fresh);
    } catch (error: any) {
      notify(error.message);
    }
  });

  // ---- other-server sessions (cross-server groups) ----
  // Lists every server the sidecar has dialed before (spec cache); picking
  // one opens a new shell on it as a tab, ready to group against.
  async function toggleServerPop(): Promise<void> {
    if (!serverPop.hidden) {
      serverPop.hidden = true;
      return;
    }
    serverPop.innerHTML = `<div class="server-pop-loading">…</div>`;
    serverPop.hidden = false;
    try {
      const list = await api<{ connections: { connectionId: string; name: string; host: string; username: string }[] }>(
        "connection/list",
        {},
        30000,
      );
      const others = list.connections.filter((item) => item.connectionId !== connectionId);
      serverPop.innerHTML = "";
      if (others.length === 0) {
        const empty = document.createElement("div");
        empty.className = "server-pop-empty";
        empty.textContent = t("noOtherServers");
        serverPop.append(empty);
        return;
      }
      for (const item of others) {
        const entry = document.createElement("button");
        entry.className = "server-pop-item";
        entry.innerHTML = `<span class="server-pop-name"></span><span class="server-pop-sub"></span>`;
        entry.querySelector<HTMLElement>(".server-pop-name")!.textContent = item.name || item.host;
        entry.querySelector<HTMLElement>(".server-pop-sub")!.textContent = item.username
          ? `${item.username}@${item.host}`
          : item.host;
        entry.addEventListener("click", async () => {
          serverPop.hidden = true;
          try {
            const created = await newShellForConnection(
              item.connectionId,
              tabs[activeIndex()]?.term?.cols() ?? 80,
              tabs[activeIndex()]?.term?.rows() ?? 24,
            );
            tabs.push({
              shellId: created.shellId,
              title: item.name || item.host,
              tooltip: `${item.username}@${item.host}`,
              connectionId: item.connectionId,
              sub: item.host,
              fresh: true,
            });
            focusTab(tabs.length - 1);
          } catch (error: any) {
            notify(error.message);
          }
        });
        serverPop.append(entry);
      }
    } catch (error: any) {
      serverPop.hidden = true;
      notify(error.message);
    }
  }
  serverButton.addEventListener("click", () => void toggleServerPop());

  // ---- quick commands bar ----
  // Snippets live in the plugin-wide settings blob (added/removed right
  // here; the settings.changed broadcast keeps every surface in sync).
  // Clicking one sends it to the focused terminal with an Enter.
  function saveSnippets(): void {
    saveStyleSettings(styleState);
    renderSnipsPop();
  }
  function renderSnipsPop(): void {
    snipsPop.innerHTML = "";
    if (styleState.snippets.length === 0) {
      const empty = document.createElement("div");
      empty.className = "server-pop-empty";
      empty.textContent = t("snippetsEmpty");
      snipsPop.append(empty);
    }
    for (const snippet of styleState.snippets) {
      const row = document.createElement("button");
      row.className = "server-pop-item snip-row";
      row.innerHTML = `<span class="server-pop-name"></span><span class="server-pop-sub"></span><span class="snip-del" title="${t("remove")}">${icon("close", 11)}</span>`;
      row.querySelector<HTMLElement>(".server-pop-name")!.textContent = snippet.name;
      const cmdEl = row.querySelector<HTMLElement>(".server-pop-sub")!;
      cmdEl.textContent = snippet.cmd.length > 48 ? `${snippet.cmd.slice(0, 48)}…` : snippet.cmd;
      row.addEventListener("click", (event) => {
        if ((event.target as HTMLElement).closest(".snip-del")) {
          event.stopPropagation();
          styleState.snippets = styleState.snippets.filter((entry) => entry.id !== snippet.id);
          saveSnippets();
          return;
        }
        snipsPop.hidden = true;
        const tab = tabs[activeIndex()];
        if (!tab?.shellId) return;
        sendBinary(`term/${tab.shellId}`, encoder.encode(`${snippet.cmd}\r`));
        notify(t("snippetSent"), "info");
      });
      snipsPop.append(row);
    }
    const addRow = document.createElement("div");
    addRow.className = "snip-add";
    const name = document.createElement("input");
    name.type = "text";
    name.placeholder = t("snippetNamePlaceholder");
    name.maxLength = 24;
    name.spellcheck = false;
    const cmd = document.createElement("input");
    cmd.type = "text";
    cmd.placeholder = t("snippetCmdPlaceholder");
    cmd.spellcheck = false;
    const add = document.createElement("button");
    add.type = "button";
    add.className = "btn ghost";
    add.textContent = t("snippetAdd");
    const commit = (): void => {
      const snippetName = name.value.trim();
      const snippetCmd = cmd.value.trim();
      if (!snippetName || !snippetCmd) return;
      styleState.snippets = [
        ...styleState.snippets,
        { id: `snip-${Date.now()}`, name: snippetName, cmd: snippetCmd },
      ];
      saveSnippets();
    };
    add.addEventListener("click", commit);
    cmd.addEventListener("keydown", (event) => {
      event.stopPropagation();
      if (event.key === "Enter") commit();
      else if (event.key === "Escape") (snipsPop.hidden = true);
    });
    name.addEventListener("keydown", (event) => event.stopPropagation());
    addRow.append(name, cmd, add);
    snipsPop.append(addRow);
  }
  snipsButton.addEventListener("click", () => {
    if (!snipsPop.hidden) {
      snipsPop.hidden = true;
      return;
    }
    renderSnipsPop();
    snipsPop.hidden = false;
    snipsPop.querySelector<HTMLInputElement>(".snip-add input")?.focus();
  });

  // ---- server monitor (CPU / memory / disks, polled while open) ----
  // One exec round trip per sample on the focused tab's connection; the CPU
  // sparkline keeps the last 60 samples (≈5 min at the 5s poll).
  interface MonitorDisk { mount: string; totalKb: number; usedKb: number; usePct: number }
  interface MonitorSample { os: string; cpu: number; memTotal: number; memUsed: number; disks: MonitorDisk[] }
  let monitorTimer = 0;
  const cpuHistory: number[] = [];

  function humanKib(kib: number): string {
    if (!kib) return "0";
    if (kib >= 1024 * 1024 * 1024) return `${(kib / 1024 / 1024 / 1024).toFixed(1)} TiB`;
    if (kib >= 1024 * 1024) return `${(kib / 1024 / 1024).toFixed(1)} GiB`;
    if (kib >= 1024) return `${(kib / 1024).toFixed(0)} MiB`;
    return `${kib} KiB`;
  }

  function bar(pct: number): HTMLElement {
    const wrap = document.createElement("div");
    wrap.className = "mon-bar";
    const fill = document.createElement("span");
    fill.style.width = `${Math.min(100, Math.max(0, pct))}%`;
    if (pct >= 85) fill.classList.add("hot");
    wrap.append(fill);
    return wrap;
  }

  function drawCpuSpark(canvas: HTMLCanvasElement): void {
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    const w = canvas.width;
    const h = canvas.height;
    ctx.clearRect(0, 0, w, h);
    ctx.strokeStyle = "#3fb950";
    ctx.lineWidth = 1.5;
    ctx.beginPath();
    const points = cpuHistory.slice(-60);
    points.forEach((value, index) => {
      const x = (index / 59) * w;
      const y = h - (Math.min(100, Math.max(0, value)) / 100) * (h - 2) - 1;
      if (index === 0) ctx.moveTo(x, y);
      else ctx.lineTo(x, y);
    });
    ctx.stroke();
  }

  function renderMonitor(sample: MonitorSample): void {
    if (sample.os === "unsupported") {
      monitorPop.innerHTML = `<div class="server-pop-empty">${t("monitorUnsupported")}</div>`;
      return;
    }
    if (typeof sample.cpu === "number" && sample.cpu >= 0) {
      cpuHistory.push(sample.cpu);
      if (cpuHistory.length > 60) cpuHistory.shift();
    }
    monitorPop.innerHTML = "";
    const cpuRow = document.createElement("div");
    cpuRow.className = "mon-row";
    cpuRow.innerHTML = `<span class="mon-label"></span><span class="mon-value"></span>`;
    cpuRow.querySelector<HTMLElement>(".mon-label")!.textContent = t("monitorCpu");
    cpuRow.querySelector<HTMLElement>(".mon-value")!.textContent =
      typeof sample.cpu === "number" && sample.cpu >= 0 ? `${sample.cpu.toFixed(0)}%` : "—";
    const spark = document.createElement("canvas");
    spark.className = "mon-spark";
    spark.width = 240;
    spark.height = 30;
    cpuRow.append(spark);
    monitorPop.append(cpuRow, bar(cpuHistory[cpuHistory.length - 1] ?? 0));

    if (sample.memTotal > 0) {
      const memRow = document.createElement("div");
      memRow.className = "mon-row";
      memRow.innerHTML = `<span class="mon-label"></span><span class="mon-value"></span>`;
      memRow.querySelector<HTMLElement>(".mon-label")!.textContent = t("monitorMem");
      memRow.querySelector<HTMLElement>(".mon-value")!.textContent =
        `${humanKib(sample.memUsed)} / ${humanKib(sample.memTotal)}`;
      monitorPop.append(memRow, bar((sample.memUsed / sample.memTotal) * 100));
    }

    if (Array.isArray(sample.disks) && sample.disks.length > 0) {
      const diskHead = document.createElement("div");
      diskHead.className = "mon-row mon-disk-head";
      const headLabel = document.createElement("span");
      headLabel.className = "mon-label";
      headLabel.textContent = t("monitorDisk");
      diskHead.append(headLabel);
      monitorPop.append(diskHead);
      for (const disk of sample.disks) {
        const diskRow = document.createElement("div");
        diskRow.className = "mon-row mon-disk";
        diskRow.innerHTML = `<span class="mon-label" title=""></span><span class="mon-value"></span>`;
        const labelEl = diskRow.querySelector<HTMLElement>(".mon-label")!;
        labelEl.textContent = disk.mount;
        labelEl.title = `${humanKib(disk.usedKb)} / ${humanKib(disk.totalKb)}`;
        diskRow.querySelector<HTMLElement>(".mon-value")!.textContent = `${disk.usePct}%`;
        monitorPop.append(diskRow, bar(disk.usePct));
      }
    }
    drawCpuSpark(spark);
  }

  async function sampleMonitor(): Promise<void> {
    const target = tabs[activeIndex()]?.connectionId || connectionId;
    if (!target) return;
    try {
      const sample = await api<MonitorSample>("monitor/sample", { connectionId: target }, 20000);
      renderMonitor(sample);
    } catch (error: any) {
      if (monitorTimer) {
        window.clearInterval(monitorTimer);
        monitorTimer = 0;
      }
      monitorPop.innerHTML = `<div class="server-pop-empty">${error.message}</div>`;
    }
  }

  function closeMonitor(): void {
    monitorPop.hidden = true;
    monitorButton.classList.remove("active");
    if (monitorTimer) {
      window.clearInterval(monitorTimer);
      monitorTimer = 0;
    }
  }
  monitorButton.addEventListener("click", () => {
    if (!monitorPop.hidden) {
      closeMonitor();
      return;
    }
    cpuHistory.length = 0;
    monitorPop.hidden = false;
    monitorButton.classList.add("active");
    void sampleMonitor();
    monitorTimer = window.setInterval(() => void sampleMonitor(), 5000);
  });

  // plugin settings live on their own page; this button is only the entry
  // (the config itself is plugin-wide, never stored per session)
  root.querySelector<HTMLElement>("#btn-settings")!.addEventListener("click", () => {
    void bridge()
      .openWorkbench("com.milii.termhive.settings", { view: "settings" })
      .catch(() => undefined);
  });
  document.addEventListener("pointerdown", (event) => {
    if (!serverPop.hidden && !serverPop.contains(event.target as Node) && event.target !== serverButton) {
      serverPop.hidden = true;
    }
    if (!snipsPop.hidden && !snipsPop.contains(event.target as Node) && event.target !== snipsButton) {
      snipsPop.hidden = true;
    }
    if (!monitorPop.hidden && !monitorPop.contains(event.target as Node) && event.target !== monitorButton) {
      closeMonitor();
    }
  });

  if (hasFloating()) {
    popButton.addEventListener("click", async () => {
      try {
        await bridge().floating!.open({
          contributionId: MINI_ID,
          title: sessionName || "TermHive",
          width: 560,
          height: 380,
          resizable: true,
          context: { view: "mini", shellId: currentShellId(), connectionId, name: sessionName },
        });
      } catch (error: any) {
        notify(`${t("unsupported")}: ${error.message}`);
      }
    });
  }

  root.querySelector<HTMLElement>("#btn-reconnect")!.addEventListener("click", () => void connect());

  return () => {
    observer.disconnect();
    detachEvent();
    detachStyleEvent();
    closeMonitor();
    for (const tab of tabs) disposeTab(tab);
    root.remove();
  };
}
