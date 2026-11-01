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
    const view = new URLSearchParams(location.search).get("view");
    const context = view
      ? {
          view,
          shellId: view === "mini" ? "sh-demo" : undefined,
          connectionId: "c-demo",
          name: "demo-web-01",
        }
      : {};
    installMockBridge(context);
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
