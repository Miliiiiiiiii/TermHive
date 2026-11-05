// Dedicated settings workbench page: plugin-wide terminal style (scheme +
// log highlight rules). Lives in its own tab so configuration is clearly
// plugin-level, never part of a connection session. Edits broadcast through
// the sidecar (settings.changed) and every open terminal re-applies live.
import { t } from "../i18n";
import {
  defaultStyleSettings,
  loadStyleSettings,
  saveStyleSettings,
  onStyleSettingsChanged,
  renderStylePop,
  type StyleSettings,
} from "../styleSettings";

export async function mountSettings(): Promise<() => void> {
  const root = document.createElement("div");
  root.className = "settings-page";
  root.innerHTML = `
    <div class="settings-page-inner">
      <h2>${t("styleTitle")}</h2>
      <p class="settings-page-sub">${t("settingsPageSub")}</p>
      <div class="settings-panel" id="settings-panel"></div>
    </div>
  `;
  document.querySelector("#app")!.append(root);
  const panel = root.querySelector<HTMLElement>("#settings-panel")!;

  const styleState: StyleSettings = defaultStyleSettings();
  function handleStyleChange(next: StyleSettings): void {
    saveStyleSettings(next);
  }
  function render(): void {
    renderStylePop(panel, styleState, handleStyleChange);
  }
  void loadStyleSettings().then((next) => {
    Object.assign(styleState, next);
    render();
  });
  const offStyleEvent = onStyleSettingsChanged((next) => {
    Object.assign(styleState, next);
    render();
  });

  return () => {
    offStyleEvent();
    root.remove();
  };
}
