// Built-in terminal color schemes. "auto" follows the DBX host theme (the
// behavior of term.ts when no scheme is chosen); the rest are classic
// palettes expressed as xterm ITheme objects.
import type { ITheme } from "@xterm/xterm";

export interface SchemeDef {
  id: string;
  name: string;
  light?: boolean;
  theme: ITheme;
}

const oneDark: ITheme = {
  background: "#282c34",
  foreground: "#abb2bf",
  cursor: "#61afef",
  black: "#282c34", red: "#e06c75", green: "#98c379", yellow: "#e5c07b",
  blue: "#61afef", magenta: "#c678dd", cyan: "#56b6c2", white: "#abb2bf",
  brightBlack: "#5c6370", brightRed: "#e06c75", brightGreen: "#98c379", brightYellow: "#e5c07b",
  brightBlue: "#61afef", brightMagenta: "#c678dd", brightCyan: "#56b6c2", brightWhite: "#ffffff",
};

const dracula: ITheme = {
  background: "#282a36",
  foreground: "#f8f8f2",
  cursor: "#ff79c6",
  black: "#000000", red: "#ff5555", green: "#50fa7b", yellow: "#f1fa8c",
  blue: "#bd93f9", magenta: "#ff79c6", cyan: "#8be9fd", white: "#bfbfbf",
  brightBlack: "#4d4d4d", brightRed: "#ff6e67", brightGreen: "#5af78e", brightYellow: "#f4f99d",
  brightBlue: "#caa9fa", brightMagenta: "#ff92d0", brightCyan: "#9aedfe", brightWhite: "#e6e6e6",
};

const solarizedDark: ITheme = {
  background: "#002b36",
  foreground: "#839496",
  cursor: "#93a1a1",
  black: "#073642", red: "#dc322f", green: "#859900", yellow: "#b58900",
  blue: "#268bd2", magenta: "#d33682", cyan: "#2aa198", white: "#eee8d5",
  brightBlack: "#002b36", brightRed: "#cb4b16", brightGreen: "#586e75", brightYellow: "#657b83",
  brightBlue: "#839496", brightMagenta: "#6c71c4", brightCyan: "#93a1a1", brightWhite: "#fdf6e3",
};

const nord: ITheme = {
  background: "#2e3440",
  foreground: "#d8dee9",
  cursor: "#d8dee9",
  black: "#3b4252", red: "#bf616a", green: "#a3be8c", yellow: "#ebcb8b",
  blue: "#81a1c1", magenta: "#b48ead", cyan: "#88c0d0", white: "#e5e9f0",
  brightBlack: "#4c566a", brightRed: "#bf616a", brightGreen: "#a3be8c", brightYellow: "#ebcb8b",
  brightBlue: "#81a1c1", brightMagenta: "#b48ead", brightCyan: "#8fbcbb", brightWhite: "#eceff4",
};

const githubLight: ITheme = {
  background: "#ffffff",
  foreground: "#24292e",
  cursor: "#044289",
  black: "#24292e", red: "#d73a49", green: "#28a745", yellow: "#dbab09",
  blue: "#0366d6", magenta: "#5a32a3", cyan: "#05a5ba", white: "#e1e4e8",
  brightBlack: "#959da5", brightRed: "#cb2431", brightGreen: "#22863a", brightYellow: "#b08800",
  brightBlue: "#005cc5", brightMagenta: "#5a32a3", brightCyan: "#3192aa", brightWhite: "#d1d5da",
};

export const SCHEMES: SchemeDef[] = [
  { id: "oneDark", name: "One Dark", theme: oneDark },
  { id: "dracula", name: "Dracula", theme: dracula },
  { id: "solarizedDark", name: "Solarized Dark", theme: solarizedDark },
  { id: "nord", name: "Nord", theme: nord },
  { id: "githubLight", name: "GitHub Light", light: true, theme: githubLight },
];

export function findScheme(id: string): SchemeDef | undefined {
  return SCHEMES.find((scheme) => scheme.id === id);
}
