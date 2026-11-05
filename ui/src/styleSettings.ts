// Plugin-wide style settings (terminal scheme + log-highlight rules).
// One source of truth: the sidecar's settings.json. Every UI surface (each
// connection tab, the floating mini window, the launcher home) loads the
// same blob, saves through the same debounced writer and re-applies live
// when the sidecar broadcasts settings.changed — so the config behaves as
// plugin-level, never per-session.
import { icon } from "./icon";
import { t } from "./i18n";
import { DEFAULT_RULES, RULE_COLORS, RULE_COLOR_HEX, type HighlightRule } from "./highlight";
import { SCHEMES } from "./theme";
import { api, onSidecarEvent } from "./rpc";

export interface StyleSettings {
  scheme: string;
  highlight: boolean;
  rules: HighlightRule[];
  /** behavior toggles share the same settings blob (settings/put merges) */
  autoReconnect: boolean;
  /** give up auto-reconnect after this many failed redials (backend clamps 1..500) */
  autoReconnectMaxAttempts: number;
  sessionLog: boolean;
  /** auto-remove finished transfer queue rows after this many seconds */
  transferAutoClose: boolean;
  transferAutoCloseDelay: number;
}

export function defaultStyleSettings(): StyleSettings {
  return {
    scheme: "auto",
    highlight: true,
    rules: DEFAULT_RULES.map((rule) => ({ ...rule })),
    autoReconnect: true,
    autoReconnectMaxAttempts: 20,
    sessionLog: false,
    transferAutoClose: true,
    transferAutoCloseDelay: 3,
  };
}

type StoredSettings = {
  scheme?: string;
  highlight?: boolean;
  rules?: HighlightRule[];
  autoReconnect?: boolean;
  autoReconnectMaxAttempts?: number;
  sessionLog?: boolean;
  transferAutoClose?: boolean;
  transferAutoCloseDelay?: number;
};

function applyStored(settings: StyleSettings, stored: StoredSettings): void {
  if (typeof stored.scheme === "string" && stored.scheme) settings.scheme = stored.scheme;
  if (typeof stored.highlight === "boolean") settings.highlight = stored.highlight;
  if (Array.isArray(stored.rules) && stored.rules.length > 0) settings.rules = stored.rules;
  if (typeof stored.autoReconnect === "boolean") settings.autoReconnect = stored.autoReconnect;
  if (
    typeof stored.autoReconnectMaxAttempts === "number" &&
    Number.isFinite(stored.autoReconnectMaxAttempts)
  ) {
    settings.autoReconnectMaxAttempts = Math.min(
      500,
      Math.max(1, Math.round(stored.autoReconnectMaxAttempts)),
    );
  }
  if (typeof stored.sessionLog === "boolean") settings.sessionLog = stored.sessionLog;
  if (typeof stored.transferAutoClose === "boolean") settings.transferAutoClose = stored.transferAutoClose;
  if (typeof stored.transferAutoCloseDelay === "number" && Number.isFinite(stored.transferAutoCloseDelay)) {
    settings.transferAutoCloseDelay = Math.min(3600, Math.max(0, Math.round(stored.transferAutoCloseDelay)));
  }
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
          autoReconnectMaxAttempts: settings.autoReconnectMaxAttempts,
          sessionLog: settings.sessionLog,
          transferAutoClose: settings.transferAutoClose,
          transferAutoCloseDelay: settings.transferAutoCloseDelay,
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

// exactly one color palette may be open at a time; module-level so opening
// another swatch (or any full re-render) tears the previous one down with its
// document listener
let activePalette: { wrap: HTMLElement; dismiss: (event: Event) => void } | null = null;
function closeColorPalette(): void {
  if (!activePalette) return;
  document.removeEventListener("pointerdown", activePalette.dismiss, true);
  activePalette.wrap.remove();
  activePalette = null;
}

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
      ...state,
      rules: state.rules.map((rule) => ({ ...rule })),
    };
    patch(next);
    Object.assign(state, next);
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
  const behaviorRow = (key: "sessionLog"): void => {
    const row = document.createElement("label");
    row.className = "settings-row";
    const box = document.createElement("input");
    box.type = "checkbox";
    box.checked = state[key];
    box.addEventListener("change", () => commit((draft) => (draft[key] = box.checked)));
    const text = document.createElement("span");
    text.textContent = t("sessionLogLabel");
    row.append(box, text);
    pop.append(row);
  };
  // auto-reconnect carries its give-up budget: after this many failed redials
  // the supervisor stops and the manual-reconnect overlay takes over (an
  // unreachable server would otherwise be retried forever)
  {
    const row = document.createElement("div");
    row.className = "settings-row";
    const box = document.createElement("input");
    box.type = "checkbox";
    box.checked = state.autoReconnect;
    box.addEventListener("change", () =>
      commit((draft) => (draft.autoReconnect = box.checked)),
    );
    const text = document.createElement("span");
    text.textContent = t("autoReconnectLabel");
    const attemptsInput = document.createElement("input");
    attemptsInput.type = "number";
    attemptsInput.className = "num";
    attemptsInput.min = "1";
    attemptsInput.max = "500";
    attemptsInput.step = "1";
    attemptsInput.value = String(state.autoReconnectMaxAttempts);
    attemptsInput.disabled = !state.autoReconnect;
    attemptsInput.title = t("autoReconnectMaxTitle");
    attemptsInput.addEventListener("change", () => {
      const value = Math.min(500, Math.max(1, Math.round(Number(attemptsInput.value) || 1)));
      attemptsInput.value = String(value);
      commit((draft) => (draft.autoReconnectMaxAttempts = value));
    });
    const attemptsUnit = document.createElement("span");
    attemptsUnit.className = "unit";
    attemptsUnit.textContent = t("attemptsUnit");
    row.append(box, text, attemptsInput, attemptsUnit);
    pop.append(row);
  }
  behaviorRow("sessionLog");
  // finished-transfer rows: auto-dismiss after N seconds (0 = immediately,
  // once the fade completes), off keeps them for manual inspection
  const closeRow = document.createElement("div");
  closeRow.className = "settings-row";
  const closeBox = document.createElement("input");
  closeBox.type = "checkbox";
  closeBox.checked = state.transferAutoClose;
  closeBox.addEventListener("change", () =>
    commit((draft) => (draft.transferAutoClose = closeBox.checked)),
  );
  const closeText = document.createElement("span");
  closeText.textContent = t("transferAutoCloseLabel");
  const delayInput = document.createElement("input");
  delayInput.type = "number";
  delayInput.className = "num";
  delayInput.min = "0";
  delayInput.max = "3600";
  delayInput.step = "1";
  delayInput.value = String(state.transferAutoCloseDelay);
  delayInput.disabled = !state.transferAutoClose;
  delayInput.addEventListener("change", () => {
    const value = Math.min(3600, Math.max(0, Math.round(Number(delayInput.value) || 0)));
    delayInput.value = String(value);
    commit((draft) => (draft.transferAutoCloseDelay = value));
  });
  const delayUnit = document.createElement("span");
  delayUnit.className = "unit";
  delayUnit.textContent = t("secondsUnit");
  closeRow.append(closeBox, closeText, delayInput, delayUnit);
  pop.append(closeRow);

  section(t("rulesLabel"));
  state.rules.forEach((rule, index) => {
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
    // color as a swatch + palette popover instead of a text dropdown: the
    // rule colors are fixed ANSI presets, so seven blocks say more than seven
    // names ever could
    const color = document.createElement("button");
    color.type = "button";
    color.className = "rule-color";
    color.title = labels[rule.color] ?? rule.color;
    color.innerHTML = `<span class="swatch" style="background:${RULE_COLOR_HEX[rule.color]}"></span>`;
    color.addEventListener("click", () => {
      if (activePalette?.wrap.parentElement === color) {
        closeColorPalette();
        return;
      }
      closeColorPalette();
      const palette = document.createElement("div");
      palette.className = "rule-palette";
      for (const value of RULE_COLORS) {
        const swatchButton = document.createElement("button");
        swatchButton.type = "button";
        swatchButton.className = "sw";
        if (value === rule.color) swatchButton.classList.add("active");
        swatchButton.style.background = RULE_COLOR_HEX[value];
        swatchButton.title = labels[value] ?? value;
        swatchButton.addEventListener("click", () => {
          closeColorPalette();
          commit((draft) => (draft.rules[index].color = value));
        });
        palette.append(swatchButton);
      }
      const dismiss = (event: Event): void => {
        // the row was re-rendered out from under us: just clean up
        if (!palette.isConnected) {
          closeColorPalette();
          return;
        }
        const target = event.target as Node;
        if (palette.contains(target) || color.contains(target)) return;
        closeColorPalette();
      };
      document.addEventListener("pointerdown", dismiss, true);
      activePalette = { wrap: palette, dismiss };
      color.append(palette);
    });
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
