// Floating mini window: a compact always-on-top terminal attached to a shell.
import { bridge, hasFloating } from "../host";
import { icon } from "../icon";
import { t } from "../i18n";
import { createTerminal, type TerminalHandle } from "../term";
import { Highlighter } from "../highlight";
import { loadStyleSettings, onStyleSettingsChanged } from "../styleSettings";
import { api, listShells, onBinaryChannel, onSidecarEvent, sendBinary } from "../rpc";

export interface MiniContext {
  shellId?: string;
  connectionId?: string;
  name?: string;
}

export async function mountMini(context: MiniContext): Promise<() => void> {
  const root = document.createElement("div");
  root.className = "mini";
  root.innerHTML = `
    <header class="mini-bar" id="drag">
      <span class="dot live"></span>
      <span class="title"></span>
      <span class="spacer"></span>
      <button class="icon-btn" id="btn-close" title="${t("cancel")}">${icon("close", 14)}</button>
    </header>
    <div class="mini-term" id="term"></div>
  `;
  document.querySelector("#app")!.append(root);
  const title = root.querySelector<HTMLElement>(".title")!;
  const termWrap = root.querySelector<HTMLElement>("#term")!;

  let shellId = context.shellId ?? "";
  if (!shellId) {
    const shells = (await listShells().catch(() => [])).filter((shell) => !shell.closed && shell.kind === "ssh");
    shellId = shells[0]?.shellId ?? "";
    if (!shellId) {
      title.textContent = t("noLive");
      return () => root.remove();
    }
  }
  if (context.name) title.textContent = context.name;

  // plugin-wide style settings: same blob as every other surface, and live
  // updates when one of them changes it (settings.changed broadcast)
  const styleState = await loadStyleSettings();
  const term: TerminalHandle = createTerminal(termWrap, styleState.scheme);
  const highlighter = new Highlighter({ enabled: styleState.highlight, rules: styleState.rules });
  const offBinary = onBinaryChannel(`term/${shellId}`, (data) => term.write(highlighter.process(data)));
  const offStyleEvent = onStyleSettingsChanged((next) => {
    term.setScheme(next.scheme);
    highlighter.setEnabled(next.highlight);
    highlighter.setRules(next.rules);
  });
  term.onData((data) => sendBinary(`term/${shellId}`, new TextEncoder().encode(data)));
  const offEvent = onSidecarEvent((message) => {
    if (message.method === "shell.closed" && message.params?.shellId === shellId) {
      title.textContent = `${context.name ?? ""} · ${t("disconnected")}`;
      root.querySelector<HTMLElement>(".dot")!.className = "dot dead";
    }
  });
  await api("session/attach", { shellId }, 30000).catch(() => undefined);
  // every session/resize makes Windows OpenSSH re-emit the whole screen, so
  // only send it when the dims actually changed (same fix as the main view)
  let sentCols = 0;
  let sentRows = 0;
  const sendResizeIfChanged = (): void => {
    if (sentCols === term.cols() && sentRows === term.rows()) return;
    sentCols = term.cols();
    sentRows = term.rows();
    void api("session/resize", { shellId, cols: sentCols, rows: sentRows }).catch(() => undefined);
  };
  sendResizeIfChanged();

  const observer = new ResizeObserver(() => {
    term.fit();
    sendResizeIfChanged();
  });
  observer.observe(termWrap);

  root.querySelector<HTMLElement>("#btn-close")!.addEventListener("click", () => {
    if (hasFloating()) bridge().floating!.close().catch(() => undefined);
  });

  // Drag the borderless window by its header.
  if (hasFloating()) {
    const bar = root.querySelector<HTMLElement>("#drag")!;
    let armed = false;
    bar.addEventListener("pointerdown", () => {
      armed = true;
    });
    bar.addEventListener("pointermove", (event) => {
      if (armed && event.movementX + event.movementY !== 0) {
        armed = false;
        bridge().floating!.beginDrag();
      }
    });
    bar.addEventListener("pointerup", () => {
      armed = false;
    });
  }

  return () => {
    observer.disconnect();
    offBinary();
    offEvent();
    offStyleEvent();
    term.dispose();
    root.remove();
  };
}
