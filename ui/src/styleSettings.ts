// Plugin-wide style settings (terminal scheme + log-highlight rules).
// One source of truth: the sidecar's settings.json. Every UI surface (each
// connection tab, the floating mini window, the launcher home) loads the
// same blob, saves through the same debounced writer and re-applies live
// when the sidecar broadcasts settings.changed — so the config behaves as
// plugin-level, never per-session.
import { icon } from "./icon";
import { t } from "./i18n";
import { notify } from "./notify";
import { DEFAULT_RULES, RULE_COLORS, RULE_COLOR_HEX, isRuleColor, type HighlightRule } from "./highlight";
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
  if (Array.isArray(stored.rules)) {
    // colors may be preset names (legacy) or wheel-picked #rrggbb; anything
    // else in a hand-edited settings.json falls back to a sane default
    const rules = stored.rules
      .filter((rule) => rule && typeof rule.pattern === "string")
      .map((rule) => ({ ...rule, color: isRuleColor(rule.color) ? rule.color : "cyan" }));
    if (rules.length > 0) settings.rules = rules;
  }
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

// ---- color wheel picker ----
//
// HSV wheel (hue by angle, saturation by radius) plus a brightness strip, a
// hex field and — where the host allows it — the EyeDropper API for picking
// straight off the screen. Presets stay one click away: they commit the
// preset NAME, keeping the tuned bright-ANSI codes; wheel picks commit
// #rrggbb, rendered as 24-bit truecolor by the highlighter.

function hsvToRgb(h: number, s: number, v: number): [number, number, number] {
  const channel = (n: number): number => {
    const k = (n + h / 60) % 6;
    return Math.round((v - v * s * Math.max(0, Math.min(k, 4 - k, 1))) * 255);
  };
  return [channel(5), channel(3), channel(1)];
}

function rgbToHex(r: number, g: number, b: number): string {
  return `#${[r, g, b].map((c) => c.toString(16).padStart(2, "0")).join("")}`;
}

function hexToHsv(hex: string): [number, number, number] {
  const m = /^#?([0-9a-f]{6})$/i.exec(hex.trim());
  const value = m ? parseInt(m[1], 16) : 0x29b8db;
  const r = ((value >> 16) & 255) / 255;
  const g = ((value >> 8) & 255) / 255;
  const b = (value & 255) / 255;
  const max = Math.max(r, g, b);
  const min = Math.min(r, g, b);
  const d = max - min;
  let h = 0;
  if (d > 0) {
    if (max === r) h = 60 * (((g - b) / d) % 6);
    else if (max === g) h = 60 * ((b - r) / d + 2);
    else h = 60 * ((r - g) / d + 4);
  }
  if (h < 0) h += 360;
  return [h, max === 0 ? 0 : d / max, max];
}

// openColorWheel mounts the picker under `anchor`. onPreview fires on every
// drag step (live-update the rule's swatch, no commit), onDone fires once
// with the final value (hex from the wheel, preset name from the preset row)
// and closes the picker.
function openColorWheel(
  anchor: HTMLElement,
  initialHex: string,
  onPreview: (hex: string) => void,
  onDone: (color: string) => void,
): void {
  closeColorPalette();
  const state = { h: 0, s: 0, v: 1 };
  [state.h, state.s, state.v] = hexToHsv(initialHex);

  const wrap = document.createElement("div");
  wrap.className = "rule-palette color-wheel-pop";

  const wheelBox = document.createElement("div");
  wheelBox.className = "wheel-box";
  const pixels = 168;
  const canvas = document.createElement("canvas");
  canvas.width = pixels * 2;
  canvas.height = pixels * 2;
  const marker = document.createElement("div");
  marker.className = "wheel-marker";
  wheelBox.append(canvas, marker);

  const drawWheel = (): void => {
    const ctx = canvas.getContext("2d");
    if (!ctx) return;
    const size = canvas.width;
    const center = size / 2;
    const image = ctx.createImageData(size, size);
    for (let y = 0; y < size; y++) {
      for (let x = 0; x < size; x++) {
        const dx = x - center;
        const dy = y - center;
        const radius = Math.sqrt(dx * dx + dy * dy);
        if (radius > center) continue;
        const hue = ((Math.atan2(dy, dx) * 180) / Math.PI + 360) % 360;
        const [r, g, b] = hsvToRgb(hue, radius / center, state.v);
        const at = (y * size + x) * 4;
        image.data[at] = r;
        image.data[at + 1] = g;
        image.data[at + 2] = b;
        image.data[at + 3] = 255;
      }
    }
    ctx.putImageData(image, 0, 0);
  };

  const bright = document.createElement("input");
  bright.type = "range";
  bright.className = "wheel-bright";
  bright.min = "6";
  bright.max = "100";
  bright.step = "1";
  bright.value = String(Math.round(state.v * 100));

  const hexInput = document.createElement("input");
  hexInput.type = "text";
  hexInput.className = "wheel-hex";
  hexInput.maxLength = 7;
  hexInput.spellcheck = false;

  const drop = document.createElement("button");
  drop.type = "button";
  drop.className = "icon-btn wheel-drop";
  drop.title = t("eyedropperTitle");
  drop.innerHTML = icon("dropper", 14);
  drop.addEventListener("click", () => {
    const Picker = (window as unknown as { EyeDropper?: new () => { open(): Promise<{ sRGBHex: string }> } }).EyeDropper;
    if (!Picker) {
      notify(t("eyedropperUnsupported"), "info");
      return;
    }
    new Picker()
      .open()
      .then((result) => {
        const hex = result.sRGBHex.toLowerCase();
        [state.h, state.s, state.v] = hexToHsv(hex);
        drawWheel();
        bright.value = String(Math.round(state.v * 100));
        sync();
        closeColorPalette();
        onDone(hex);
      })
      .catch(() => undefined); // user pressed Esc — keep the picker open
  });

  const presets = document.createElement("div");
  presets.className = "wheel-presets";
  const presetLabels = colorLabel();
  for (const name of RULE_COLORS) {
    const swatchButton = document.createElement("button");
    swatchButton.type = "button";
    swatchButton.className = "sw";
    swatchButton.style.background = RULE_COLOR_HEX[name];
    swatchButton.title = presetLabels[name] ?? name;
    swatchButton.addEventListener("click", () => {
      closeColorPalette();
      onDone(name);
    });
    presets.append(swatchButton);
  }

  const sync = (): void => {
    const hex = rgbToHex(...hsvToRgb(state.h, state.s, state.v));
    hexInput.value = hex;
    const angle = (state.h * Math.PI) / 180;
    const center = pixels / 2;
    marker.style.left = `${center + Math.cos(angle) * state.s * center}px`;
    marker.style.top = `${center + Math.sin(angle) * state.s * center}px`;
    onPreview(hex);
  };

  let dragging = false;
  const fromPointer = (event: PointerEvent): void => {
    const rect = canvas.getBoundingClientRect();
    const dx = event.clientX - rect.left - rect.width / 2;
    const dy = event.clientY - rect.top - rect.height / 2;
    const radius = Math.min(Math.sqrt(dx * dx + dy * dy), rect.width / 2);
    state.h = ((Math.atan2(dy, dx) * 180) / Math.PI + 360) % 360;
    state.s = radius / (rect.width / 2);
    sync();
  };
  canvas.addEventListener("pointerdown", (event) => {
    dragging = true;
    canvas.setPointerCapture(event.pointerId);
    fromPointer(event);
  });
  canvas.addEventListener("pointermove", (event) => {
    if (dragging) fromPointer(event);
  });
  canvas.addEventListener("pointerup", (event) => {
    dragging = false;
    canvas.releasePointerCapture(event.pointerId);
    closeColorPalette();
    onDone(rgbToHex(...hsvToRgb(state.h, state.s, state.v)));
  });

  bright.addEventListener("input", () => {
    state.v = Number(bright.value) / 100;
    drawWheel();
    sync();
  });
  hexInput.addEventListener("change", () => {
    const candidate = hexInput.value.trim().startsWith("#")
      ? hexInput.value.trim()
      : `#${hexInput.value.trim()}`;
    if (!/^#[0-9a-f]{6}$/i.test(candidate)) {
      sync(); // revert the field to the current color
      return;
    }
    closeColorPalette();
    onDone(candidate.toLowerCase());
  });

  const hexRow = document.createElement("div");
  hexRow.className = "wheel-row";
  hexRow.append(hexInput, drop);

  wrap.append(wheelBox, bright, hexRow, presets);
  drawWheel();
  sync();

  const dismiss = (event: Event): void => {
    if (!wrap.isConnected) {
      closeColorPalette();
      return;
    }
    const target = event.target as Node;
    if (wrap.contains(target) || anchor.contains(target)) return;
    closeColorPalette();
  };
  document.addEventListener("pointerdown", dismiss, true);
  activePalette = { wrap, dismiss };
  anchor.append(wrap);
}

// renderStylePop builds the whole settings panel into `pop` as a stack of
// setting cards mirroring the DBX host's design language (its shadcn-style
// settings): one rounded bordered card per setting — label + muted
// description on the left, control on the right — with the host's pill
// switch proportions (32×18, 24×14 sm), h-8 selects and h-7 rule inputs.
// Every color and radius comes from the injected theme tokens, so the page
// follows the host theme for free. Mutations clone the state and report
// through onChanged; the broadcast then updates every other surface.
export function renderStylePop(
  pop: HTMLElement,
  state: StyleSettings,
  onChanged: (next: StyleSettings) => void,
): void {
  const labels = colorLabel();
  pop.innerHTML = "";
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

  // ---- small builders -----------------------------------------------------

  // host-style pill switch (checkbox input stays in the DOM for a11y/focus)
  const makeSwitch = (checked: boolean, onChange: (value: boolean) => void, small = false): HTMLLabelElement => {
    const sw = document.createElement("label");
    sw.className = small ? "th-switch sm" : "th-switch";
    const input = document.createElement("input");
    input.type = "checkbox";
    input.checked = checked;
    input.addEventListener("change", () => onChange(input.checked));
    const track = document.createElement("span");
    track.className = "sw-track";
    const thumb = document.createElement("span");
    thumb.className = "sw-thumb";
    track.append(thumb);
    sw.append(input, track);
    return sw;
  };

  const settingCard = (label: string, desc: string, control: HTMLElement | null): HTMLElement => {
    const card = document.createElement("div");
    card.className = "stg-card";
    const text = document.createElement("div");
    text.className = "stg-text";
    const name = document.createElement("div");
    name.className = "stg-label";
    name.textContent = label;
    const hint = document.createElement("div");
    hint.className = "stg-desc";
    hint.textContent = desc;
    text.append(name, hint);
    card.append(text);
    if (control) {
      const ctl = document.createElement("div");
      ctl.className = "stg-ctl";
      ctl.append(control);
      card.append(ctl);
    }
    pop.append(card);
    return card;
  };

  const numberInput = (
    value: number,
    min: number,
    max: number,
    title: string,
    onChange: (value: number) => void,
  ): HTMLInputElement => {
    const input = document.createElement("input");
    input.type = "number";
    input.className = "stg-num";
    input.min = String(min);
    input.max = String(max);
    input.step = "1";
    input.value = String(value);
    input.title = title;
    input.addEventListener("change", () => {
      const clamped = Math.min(max, Math.max(min, Math.round(Number(input.value) || min)));
      input.value = String(clamped);
      onChange(clamped);
    });
    return input;
  };

  const unitSpan = (text: string): HTMLElement => {
    const el = document.createElement("span");
    el.className = "stg-unit";
    el.textContent = text;
    return el;
  };

  // ---- cards ---------------------------------------------------------------

  const schemeSelect = document.createElement("select");
  schemeSelect.innerHTML =
    `<option value="auto">${t("schemeAuto")}</option>` +
    SCHEMES.map((def) => `<option value="${def.id}">${def.name}</option>`).join("");
  schemeSelect.value = state.scheme;
  schemeSelect.addEventListener("change", () => commit((draft) => (draft.scheme = schemeSelect.value)));
  settingCard(t("schemeLabel"), t("schemeDesc"), schemeSelect);

  // auto-reconnect carries its give-up budget: after this many failed redials
  // the supervisor stops and the manual-reconnect overlay takes over (an
  // unreachable server would otherwise be retried forever)
  {
    const attempts = numberInput(
      state.autoReconnectMaxAttempts,
      1,
      500,
      t("autoReconnectMaxTitle"),
      (value) => commit((draft) => (draft.autoReconnectMaxAttempts = value)),
    );
    attempts.disabled = !state.autoReconnect;
    const ctl = document.createElement("div");
    ctl.className = "stg-ctl";
    ctl.append(
      makeSwitch(state.autoReconnect, (value) => commit((draft) => (draft.autoReconnect = value))),
      attempts,
      unitSpan(t("attemptsUnit")),
    );
    settingCard(t("autoReconnectLabel"), t("autoReconnectDesc"), null).append(ctl);
  }
  {
    const delay = numberInput(
      state.transferAutoCloseDelay,
      0,
      3600,
      t("transferAutoCloseDesc"),
      (value) => commit((draft) => (draft.transferAutoCloseDelay = value)),
    );
    delay.disabled = !state.transferAutoClose;
    const ctl = document.createElement("div");
    ctl.className = "stg-ctl";
    ctl.append(
      makeSwitch(state.transferAutoClose, (value) => commit((draft) => (draft.transferAutoClose = value))),
      delay,
      unitSpan(t("secondsUnit")),
    );
    settingCard(t("transferAutoCloseLabel"), t("transferAutoCloseDesc"), null).append(ctl);
  }

  settingCard(
    t("sessionLogLabel"),
    t("sessionLogDesc"),
    makeSwitch(state.sessionLog, (value) => commit((draft) => (draft.sessionLog = value))),
  );

  // ---- highlight rules: one wide card, master switch in the header --------

  const rulesCard = document.createElement("div");
  rulesCard.className = "stg-card stg-rules";
  const rulesHead = document.createElement("div");
  rulesHead.className = "stg-rules-head";
  const rulesText = document.createElement("div");
  rulesText.className = "stg-text";
  const rulesName = document.createElement("div");
  rulesName.className = "stg-label";
  rulesName.textContent = t("rulesLabel");
  const rulesHint = document.createElement("div");
  rulesHint.className = "stg-desc";
  rulesHint.textContent = t("highlightDesc");
  rulesText.append(rulesName, rulesHint);
  rulesHead.append(rulesText, makeSwitch(state.highlight, (value) => commit((draft) => (draft.highlight = value))));
  rulesCard.append(rulesHead);

  const modeHint = document.createElement("div");
  modeHint.className = "stg-desc stg-rules-mode";
  modeHint.textContent = t("rulesDesc");
  rulesCard.append(modeHint);

  state.rules.forEach((rule, index) => {
    const row = document.createElement("div");
    row.className = "rule-row";
    row.append(
      makeSwitch(rule.enabled, (value) => commit((draft) => (draft.rules[index].enabled = value)), true),
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
    // color as a swatch opening the wheel picker: presets stay one click
    // away, custom picks ride 24-bit truecolor
    const color = document.createElement("button");
    color.type = "button";
    color.className = "rule-color";
    const ruleHex = rule.color.startsWith("#")
      ? rule.color
      : (RULE_COLOR_HEX[rule.color] ?? "#29b8db");
    color.title = labels[rule.color] ?? rule.color;
    color.innerHTML = `<span class="swatch" style="background:${ruleHex}"></span>`;
    color.addEventListener("click", () => {
      if (activePalette?.wrap.parentElement === color) {
        closeColorPalette();
        return;
      }
      openColorWheel(
        color,
        ruleHex,
        (hex) => {
          // live preview while dragging — no commit, no re-render
          const swatch = color.querySelector<HTMLElement>(".swatch");
          if (swatch) swatch.style.background = hex;
        },
        (picked) => commit((draft) => (draft.rules[index].color = picked)),
      );
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
    row.append(name, pattern, color, mode, del);
    rulesCard.append(row);
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
  rulesCard.append(actions);
  pop.append(rulesCard);
}
