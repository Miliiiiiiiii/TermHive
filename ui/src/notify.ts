// Sandbox-safe replacements for alert/confirm/prompt: the DBX sandbox blocks
// native modal dialogs, so the UI must render its own.
import { icon } from "./icon";
import { t } from "./i18n";

function ensureHost(): HTMLElement {
  let host = document.querySelector<HTMLElement>(".notice-host");
  if (!host) {
    host = document.createElement("div");
    host.className = "notice-host";
    document.body.append(host);
  }
  return host;
}

export function notify(message: string, kind: "error" | "info" = "error"): void {
  const host = ensureHost();
  const item = document.createElement("div");
  item.className = `notice ${kind}`;
  item.textContent = message;
    const close = document.createElement("button");
    close.className = "notice-close";
    close.innerHTML = icon("close", 13);
  close.addEventListener("click", () => item.remove());
  item.append(close);
  host.append(item);
  setTimeout(() => item.remove(), kind === "error" ? 8000 : 3000);
}

export function confirmBox(message: string): Promise<boolean> {
  return new Promise((resolve) => {
    const host = ensureHost();
    const item = document.createElement("div");
    item.className = "notice confirm";
    item.textContent = message;
    const yes = document.createElement("button");
    yes.className = "btn small";
    yes.textContent = t("ok");
    const no = document.createElement("button");
    no.className = "btn ghost small";
    no.textContent = t("cancel");
    yes.addEventListener("click", () => {
      item.remove();
      resolve(true);
    });
    no.addEventListener("click", () => {
      item.remove();
      resolve(false);
    });
    item.append(yes, no);
    host.append(item);
  });
}

// only one prompt may exist at a time: repeated triggers replace (cancel)
// the previous one instead of stacking boxes that never go away
let activePrompt: (() => void) | null = null;

export function promptBox(message: string, initial = ""): Promise<string | null> {
  activePrompt?.();
  return new Promise((resolve) => {
    const host = ensureHost();
    const item = document.createElement("div");
    item.className = "notice confirm";
    const label = document.createElement("span");
    label.textContent = message;
    const input = document.createElement("input");
    input.value = initial;
    const ok = document.createElement("button");
    ok.className = "btn small";
    ok.textContent = t("ok");
    const cancel = document.createElement("button");
    cancel.className = "btn ghost small";
    cancel.textContent = t("cancel");
    let settled = false;
    const done = (value: string | null) => {
      if (settled) return;
      settled = true;
      if (activePrompt === close) activePrompt = null;
      item.remove();
      resolve(value);
    };
    ok.addEventListener("click", () => done(input.value));
    cancel.addEventListener("click", () => done(null));
    input.addEventListener("keydown", (event) => {
      if (event.key === "Enter") done(input.value);
      if (event.key === "Escape") done(null);
    });
    // focus leaving the box (and not landing on its own buttons) dismisses
    input.addEventListener("blur", (event) => {
      const next = event.relatedTarget as Node | null;
      if (!next || !item.contains(next)) done(null);
    });
    const close = () => done(null);
    activePrompt = close;
    item.append(label, input, ok, cancel);
    host.append(item);
    input.focus();
  });
}
