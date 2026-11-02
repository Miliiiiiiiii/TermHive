// Plugin-wide style settings (terminal scheme + log-highlight rules).
// One source of truth: the sidecar's settings.json. Every UI surface (each
// connection tab, the floating mini window, the launcher home) loads the
// same blob, saves through the same debounced writer and re-applies live
// when the sidecar broadcasts settings.changed — so the config behaves as
// plugin-level, never per-session.
import { icon } from "./icon";
import { t } from "./i18n";
import { DEFAULT_RULES, RULE_COLORS, type HighlightRule } from "./highlight";
import { SCHEMES } from "./theme";
import { api, onSidecarEvent } from "./rpc";

export interface StyleSettings {
  scheme: string;
  highlight: boolean;
  rules: HighlightRule[];
  /** behavior toggles share the same settings blob (settings/put merges) */
  autoReconnect: boolean;
  sessionLog: boolean;
}

export function defaultStyleSettings(): StyleSettings {
  return {
    scheme: "auto",
    highlight: true,
    rules: DEFAULT_RULES.map((rule) => ({ ...rule })),
    autoReconnect: true,
    sessionLog: false,
  };
}

type StoredSettings = {
  scheme?: string;
  highlight?: boolean;
  rules?: HighlightRule[];
  autoReconnect?: boolean;
  sessionLog?: boolean;
};

function applyStored(settings: StyleSettings, stored: StoredSettings): void {
  if (typeof stored.scheme === "string" && stored.scheme) settings.scheme = stored.scheme;
  if (typeof stored.highlight === "boolean") settings.highlight = stored.highlight;
  if (Array.isArray(stored.rules) && stored.rules.length > 0) settings.rules = stored.rules;
  if (typeof stored.autoReconnect === "boolean") settings.autoReconnect = stored.autoReconnect;
  if (typeof stored.sessionLog === "boolean") settings.sessionLog = stored.sessionLog;
}

export async function loadStyleSettings(): Promise<StyleSettings> {
  const settings = defaultStyleSettings();
  try {
    const result = await api<{ settings: StoredSettings }>("settings/get", {}, 15000);
    applyStored(settings, result.settings ?? {});
  } catch {
    /* first run: defaults */
  }
  return settings;
}

let saveTimer = 0;
export function saveStyleSettings(settings: StyleSettings): void {
  window.clearTimeout(saveTimer);
  saveTimer = window.setTimeout(() => {
    void api(
      "settings/put",
      {
        settings: {
          scheme: settings.scheme,
          highlight: settings.highlight,
          rules: settings.rules,
          autoReconnect: settings.autoReconnect,
          sessionLog: settings.sessionLog,
        },
      },
      15000,
    ).catch(() => undefined);
  }, 600);
}

// onStyleSettingsChanged fires whenever ANY surface saves new settings — the
// sidecar broadcasts settings.changed to every plugin iframe.
export function onStyleSettingsChanged(handler: (settings: StyleSettings) => void): () => void {
  return onSidecarEvent((message) => {
    if (message.method !== "settings.changed") return;
    const stored = (message.params?.settings ?? {}) as StoredSettings;
    const next = defaultStyleSettings();
    applyStored(next, stored);
    handler(next);
  });
}

const colorLabel = (): Record<string, string> => ({
  red: t("colorRed"),
  yellow: t("colorYellow"),
  blue: t("colorBlue"),
  green: t("colorGreen"),
  cyan: t("colorCyan"),
  magenta: t("colorMagenta"),
  dim: t("colorDim"),
});

// renderStylePop builds the whole settings panel into `pop`. Every mutation
// clones the state, reports it through onChanged (the host view applies to
// its terminals and saves — the broadcast then updates every other surface).
export function renderStylePop(
  pop: HTMLElement,
  state: StyleSettings,
  onChanged: (next: StyleSettings) => void,
): void {
  const labels = colorLabel();
  pop.innerHTML = "";
  const section = (label: string): void => {
    const head = document.createElement("div");
    head.className = "settings-head";
    head.textContent = label;
    pop.append(head);
  };
  const commit = (patch: (draft: StyleSettings) => void): void => {
    const next: StyleSettings = {
      scheme: state.scheme,
      highlight: state.highlight,
      rules: state.rules.map((rule) => ({ ...rule })),
      autoReconnect: state.autoReconnect,
      sessionLog: state.sessionLog,
    };
    patch(next);
    state.scheme = next.scheme;
    state.highlight = next.highlight;
    state.rules = next.rules;
    state.autoReconnect = next.autoReconnect;
    state.sessionLog = next.sessionLog;
    onChanged(next);
    // re-render only the reactive bits would be overkill; rebuild keeps
    // every control in sync with the state it just produced
    renderStylePop(pop, state, onChanged);
  };

  section(t("schemeLabel"));
  const schemeRow = document.createElement("label");
  schemeRow.className = "settings-row";
  const schemeSelect = document.createElement("select");
  schemeSelect.innerHTML =
    `<option value="auto">${t("schemeAuto")}</option>` +
    SCHEMES.map((def) => `<option value="${def.id}">${def.name}</option>`).join("");
  schemeSelect.value = state.scheme;
  schemeSelect.addEventListener("change", () => commit((draft) => (draft.scheme = schemeSelect.value)));
  schemeRow.append(schemeSelect);
  pop.append(schemeRow);

  section(t("highlightLabel"));
  const toggleRow = document.createElement("label");
  toggleRow.className = "settings-row";
  const highlightToggle = document.createElement("input");
  highlightToggle.type = "checkbox";
  highlightToggle.checked = state.highlight;
  highlightToggle.addEventListener("change", () =>
    commit((draft) => (draft.highlight = highlightToggle.checked)),
  );
  const toggleText = document.createElement("span");
  toggleText.textContent = t("highlightLabel");
  toggleRow.append(highlightToggle, toggleText);
  pop.append(toggleRow);

  section(t("behaviorLabel"));
  const behaviorRow = (key: "autoReconnect" | "sessionLog"): void => {
    const row = document.createElement("label");
    row.className = "settings-row";
    const box = document.createElement("input");
    box.type = "checkbox";
    box.checked = state[key];
    box.addEventListener("change", () => commit((draft) => (draft[key] = box.checked)));
    const text = document.createElement("span");
    text.textContent = key === "autoReconnect" ? t("autoReconnectLabel") : t("sessionLogLabel");
    row.append(box, text);
    pop.append(row);
  };
  behaviorRow("autoReconnect");
  behaviorRow("sessionLog");

  section(t("rulesLabel"));  state.rules.forEach((rule, index) => {
    const row = document.createElement("div");
    row.className = "rule-row";
    const enable = document.createElement("input");
    enable.type = "checkbox";
    enable.checked = rule.enabled;
    enable.addEventListener("change", () =>
      commit((draft) => (draft.rules[index].enabled = enable.checked)),
    );
    const name = document.createElement("input");
    name.type = "text";
    name.value = rule.name;
    name.className = "rule-name";
    name.maxLength = 16;
    name.addEventListener("change", () =>
      commit((draft) => (draft.rules[index].name = name.value.trim() || draft.rules[index].name)),
    );
    const pattern = document.createElement("input");
    pattern.type = "text";
    pattern.value = rule.pattern;
    pattern.className = "rule-pattern";
    pattern.placeholder = t("rulePattern");
    pattern.addEventListener("change", () => {
      try {
        new RegExp(pattern.value, "gi");
        pattern.classList.remove("bad");
      } catch {
        pattern.classList.add("bad");
      }
      commit((draft) => (draft.rules[index].pattern = pattern.value));
    });
    const color = document.createElement("select");
    color.innerHTML = RULE_COLORS.map(
      (value) => `<option value="${value}">${labels[value] ?? value}</option>`,
    ).join("");
    color.value = rule.color;
    color.addEventListener("change", () =>
      commit((draft) => (draft.rules[index].color = color.value as HighlightRule["color"])),
    );
    const mode = document.createElement("select");
    mode.innerHTML =
      `<option value="line">${t("modeLine")}</option><option value="segment">${t("modeSegment")}</option>`;
    mode.value = rule.mode;
    mode.addEventListener("change", () =>
      commit((draft) => (draft.rules[index].mode = mode.value as HighlightRule["mode"])),
    );
    const del = document.createElement("button");
    del.type = "button";
    del.className = "icon-btn";
    del.title = t("remove");
    del.innerHTML = icon("trash", 13);
    del.addEventListener("click", () =>
      commit((draft) => {
        draft.rules.splice(index, 1);
      }),
    );
    row.append(enable, name, pattern, color, mode, del);
    pop.append(row);
  });

  const actions = document.createElement("div");
  actions.className = "settings-actions";
  const add = document.createElement("button");
  add.type = "button";
  add.className = "btn ghost";
  add.textContent = t("addRule");
  add.addEventListener("click", () =>
    commit((draft) => {
      draft.rules.push({
        id: `rule-${Date.now()}`,
        name: "RULE",
        pattern: "",
        color: "cyan",
        mode: "segment",
        enabled: true,
      });
    }),
  );
  const reset = document.createElement("button");
  reset.type = "button";
  reset.className = "btn ghost";
  reset.textContent = t("resetRules");
  reset.addEventListener("click", () => {
    const defaults = defaultStyleSettings();
    commit((draft) => {
      draft.scheme = defaults.scheme;
      draft.highlight = defaults.highlight;
      draft.rules = defaults.rules;
    });
  });
  actions.append(add, reset);
  pop.append(actions);
}
