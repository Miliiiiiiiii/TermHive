// Launcher view: the plugin's landing tab. Connections live in the DBX
// connection list — this tab only surfaces live sessions to re-enter and
// links to the plugin-wide settings page (its own workbench tab).
import { t } from "../i18n";
import { listShells, openSessionTab, type ShellInfo } from "../rpc";
import { bridge } from "../host";
import { icon } from "../icon";

export function mountHome(): () => void {
  const root = document.createElement("div");
  root.className = "home launcher";
  root.innerHTML = `
    <div class="home-actions">
      <button class="icon-btn" id="btn-settings" title="${t("openSettings")}">${icon("palette", 15)}</button>
    </div>
    <div class="launcher-inner">
      <div class="launcher-hero">
        <span class="launcher-logo">${icon("terminal", 28)}</span>
        <h2>TermHive</h2>
        <p>${t("launcherHint")}</p>
      </div>
      <section class="live-panel" id="live-panel" hidden>
        <h3>${t("liveSessions")}</h3>
        <div class="live-grid" id="live-grid"></div>
      </section>
    </div>
  `;
  document.querySelector("#app")!.append(root);

  const livePanel = root.querySelector<HTMLElement>("#live-panel")!;
  const liveGrid = root.querySelector<HTMLElement>("#live-grid")!;

  // plugin-level settings live on their own page, opened like any workbench
  root.querySelector<HTMLElement>("#btn-settings")!.addEventListener("click", () => {
    void bridge()
      .openWorkbench("com.milii.termhive.settings", { view: "settings" })
      .catch(() => undefined);
  });

  let disposed = false;
  let refreshTimer = 0;

  function render(shells: ShellInfo[]): void {
    if (disposed) return;
    const live = shells.filter((shell) => !shell.closed);
    livePanel.hidden = live.length === 0;
    liveGrid.innerHTML = "";
    for (const shell of live) {
      const card = document.createElement("button");
      card.className = "live-card";
      card.innerHTML = `
        <span class="live-icon">${icon("terminal", 18)}</span>
        <span class="live-main">
          <span class="live-name"></span>
          <span class="live-sub">${shell.username ? `${shell.username}@${shell.host}` : shell.host}</span>
        </span>
        <span class="chip">${shell.kind.toUpperCase()}</span>
      `;
      (card.querySelector(".live-name") as HTMLElement).textContent = shell.name;
      card.addEventListener("click", () => {
        void openSessionTab({
          connectionId: shell.connectionId,
          shellId: shell.shellId,
          name: shell.name,
        }).catch(() => undefined);
      });
      liveGrid.append(card);
    }
  }

  async function refresh(): Promise<void> {
    try {
      render(await listShells());
    } catch {
      if (!disposed) livePanel.hidden = true;
    }
  }

  void refresh();
  refreshTimer = window.setInterval(() => void refresh(), 4000);

  return () => {
    disposed = true;
    window.clearInterval(refreshTimer);
    root.remove();
  };
}
