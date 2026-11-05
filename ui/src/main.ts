// The design system must be the first import: views render at module
// evaluation time and the stylesheet has to be in place before first paint.
import "./style.css";
import { bridge } from "./host";
import { installMux } from "./rpc";
import { installMockBridge } from "./mock";
import { mountHome } from "./views/home";
import { mountSession, type SessionContext } from "./views/session";
import { mountMini, type MiniContext } from "./views/mini";
import { mountSettings } from "./views/settings";

type Dispose = () => void;

async function mountView(context: Record<string, unknown>): Promise<Dispose> {
  switch (context.view) {
    case "session":
      return mountSession(context as unknown as SessionContext);
    case "mini":
      return mountMini(context as unknown as MiniContext);
    case "settings":
      return mountSettings();
    default: {
      // The desktop host reports which contribution this surface belongs to;
      // the manual "mini" entry therefore still lands on the mini view.
      const contribution = (bridge() as any).contributionId;
      if (typeof contribution === "string" && contribution.endsWith(".mini")) {
        return mountMini(context as unknown as MiniContext);
      }
      // Opening a saved connection supplies connectionId (+providerId /
      // connectionType) without an explicit view: that is the session tab.
      if (typeof context.connectionId === "string" && context.connectionId) {
        return mountSession(context as unknown as SessionContext);
      }
      return mountHome();
    }
  }
}

async function main(): Promise<void> {
  // Host-free preview (static server): spin up a mock bridge so every view
  // stays usable while iterating on the UI outside DBX.
  if (typeof window.dbxPlugin === "undefined") {
    const params = new URLSearchParams(location.search);
    const view = params.get("view");
    const context: Record<string, unknown> = view
      ? {
          view,
          shellId: view === "mini" ? "sh-demo" : undefined,
          connectionId: "c-demo",
          name: "demo-web-01",
        }
      : {};
    // preview deep links (screenshot refresh, quick state inspection):
    // ?files=1 opens the file panel with the session; ?follow=1 additionally
    // starts the log viewer on the demo app.log once its row renders;
    // ?download=1 clicks the app.log download button (queue demo, headless
    // DOM dumps can then watch the row finish and auto-fade away);
    // ?palette=1 opens the first rule's color palette on the settings page;
    // ?renderer=dom forces xterm's DOM renderer (headless screenshot hosts)
    if (params.get("files") === "1") context.openFiles = true;
    if (params.get("renderer") === "dom") document.documentElement.dataset.noWebgl = "1";
    installMockBridge(context);
    if (params.get("follow") === "1") {
      // poll until the demo listing renders (first paint can outrun the
      // mock's async fs/list), then start the log viewer on app.log
      const deepLink = window.setInterval(() => {
        const row = Array.from(document.querySelectorAll<HTMLTableRowElement>(".fp-table tbody tr")).find((r) =>
          r.textContent?.includes("app.log"),
        );
        if (row) {
          window.clearInterval(deepLink);
          row.querySelector<HTMLButtonElement>('button[title*="tail -f"]')?.click();
        }
      }, 300);
      window.setTimeout(() => window.clearInterval(deepLink), 8000);
    }
    if (params.get("download") === "1") {
      const deepLink = window.setInterval(() => {
        const row = Array.from(document.querySelectorAll<HTMLTableRowElement>(".fp-table tbody tr")).find((r) =>
          r.textContent?.includes("app.log"),
        );
        if (row) {
          window.clearInterval(deepLink);
          // both locales: the title is the only locale-dependent part
          row.querySelector<HTMLButtonElement>('button[title="下载"], button[title="Download"]')?.click();
        }
      }, 300);
      window.setTimeout(() => window.clearInterval(deepLink), 8000);
    }
    if (params.get("palette") === "1" && view === "settings") {
      const deepLink = window.setInterval(() => {
        const button = document.querySelector<HTMLButtonElement>(".rule-color");
        if (button) {
          window.clearInterval(deepLink);
          button.click();
        }
      }, 300);
      window.setTimeout(() => window.clearInterval(deepLink), 8000);
    }
  }
  const plugin = bridge();
  await plugin.ready;
  installMux();

  let currentKey = JSON.stringify(plugin.context ?? {});
  let disposing: Dispose | null = null;

  const render = async (): Promise<void> => {
    const app = document.querySelector<HTMLElement>("#app")!;
    disposing?.();
    disposing = null;
    app.innerHTML = "";
    disposing = await mountView(plugin.context ?? {});
  };

  await render();

  // A host may reuse one surface and push a new context (the dev host does
  // exactly that for repeated openWorkbench calls), so re-route on change.
  plugin.onContext((context) => {
    const key = JSON.stringify(context ?? {});
    if (key === currentKey) return;
    currentKey = key;
    void render();
  });
}

main().catch((error) => {
  const app = document.querySelector("#app");
  if (app) {
    app.innerHTML = `<div class="fatal">TermHive failed to start: ${String(error?.message || error)}</div>`;
  }
});
