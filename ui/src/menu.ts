// Shared fixed-position context menu (log viewer, terminals, file panel).
// Reuses the .ctx-menu/.ctx-item styles the session tab bar established.
// One floating menu at a time; closes on outside pointerdown, Escape,
// window resize/scroll, or after an item fires.
export interface MenuItem {
  label?: string;
  disabled?: boolean;
  action?: () => void;
  separator?: boolean;
}

let activeClose: (() => void) | null = null;

export function closeContextMenu(): void {
  activeClose?.();
  activeClose = null;
}

export function showContextMenu(items: MenuItem[], x: number, y: number): void {
  closeContextMenu();
  const menu = document.createElement("div");
  menu.className = "ctx-menu ctx-float";
  for (const item of items) {
    if (item.separator) {
      const sep = document.createElement("div");
      sep.className = "ctx-sep";
      menu.append(sep);
      continue;
    }
    const entry = document.createElement("button");
    entry.type = "button";
    entry.className = "ctx-item" + (item.disabled ? " disabled" : "");
    entry.disabled = item.disabled === true;
    entry.textContent = item.label ?? "";
    entry.addEventListener("click", () => {
      close();
      item.action?.();
    });
    menu.append(entry);
  }
  const close = (): void => {
    menu.remove();
    window.removeEventListener("pointerdown", onDown, true);
    window.removeEventListener("keydown", onKey, true);
    window.removeEventListener("resize", close);
    if (activeClose === close) activeClose = null;
  };
  const onDown = (event: PointerEvent): void => {
    if (!menu.contains(event.target as Node)) close();
  };
  const onKey = (event: KeyboardEvent): void => {
    if (event.key === "Escape") close();
  };
  activeClose = close;
  document.body.append(menu);
  const rect = menu.getBoundingClientRect();
  menu.style.left = `${Math.max(4, Math.min(x, window.innerWidth - rect.width - 8))}px`;
  menu.style.top = `${Math.max(4, Math.min(y, window.innerHeight - rect.height - 8))}px`;
  window.addEventListener("pointerdown", onDown, true);
  window.addEventListener("keydown", onKey, true);
  window.addEventListener("resize", close);
}
