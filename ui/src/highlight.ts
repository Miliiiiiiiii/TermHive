// Log highlighter: colorizes completed output lines by injecting ANSI color
// codes before they reach xterm — a rendering concern, so replays and
// copy-paste behave like normal terminal output.
//
// Safety model: only lines that END within the current chunk are styled; an
// unfinished trailing line passes through immediately so interactive echo and
// prompts never lag. Any chunk that looks like a full-screen application
// (cursor addressing, alternate screen, erase-display) passes through
// untouched, so vim/htop and progress redraws are never disturbed.

export interface HighlightRule {
  id: string;
  name: string;
  // JavaScript regex source, matched case-insensitively
  pattern: string;
  color: "red" | "yellow" | "blue" | "green" | "cyan" | "magenta" | "dim";
  // line = tint the whole line, segment = color the matched span only
  mode: "line" | "segment";
  enabled: boolean;
}

export interface HighlightSettings {
  enabled: boolean;
  rules: HighlightRule[];
}

export const DEFAULT_RULES: HighlightRule[] = [
  { id: "error", name: "ERROR", pattern: "\\b(ERROR|FATAL|PANIC|CRITICAL|SEVERE)\\b|严重错误", color: "red", mode: "line", enabled: true },
  { id: "exception", name: "异常/失败", pattern: "Exception|Traceback|\\bFAILED\\b|致命|失败", color: "red", mode: "line", enabled: true },
  { id: "warn", name: "WARN", pattern: "\\b(WARN|WARNING)\\b|警告", color: "yellow", mode: "line", enabled: true },
  { id: "info", name: "INFO", pattern: "\\bINFO\\b|信息", color: "blue", mode: "segment", enabled: true },
  { id: "debug", name: "DEBUG", pattern: "\\b(DEBUG|TRACE|VERBOSE)\\b|调试", color: "dim", mode: "segment", enabled: true },
  { id: "ok", name: "SUCCESS", pattern: "\\b(SUCCESS|DONE|OK)\\b|成功", color: "green", mode: "segment", enabled: true },
];

export const RULE_COLORS: HighlightRule["color"][] = ["red", "yellow", "blue", "green", "cyan", "magenta", "dim"];

// display hex per rule color (mirrors the bright ANSI variants STYLES emit)
// so the settings page can render each rule as a color swatch
export const RULE_COLOR_HEX: Record<HighlightRule["color"], string> = {
  red: "#f14c4c",
  yellow: "#f5f543",
  blue: "#3b8eea",
  green: "#23d18b",
  cyan: "#29b8db",
  magenta: "#d670d6",
  dim: "#767676",
};

// style presets: bold + bright foreground stay readable on dark and light
// backgrounds; dim only lowers intensity
const STYLES: Record<HighlightRule["color"], { start: string; end: string }> = {
  red: { start: "\x1b[1;91m", end: "\x1b[22;39m" },
  yellow: { start: "\x1b[1;93m", end: "\x1b[22;39m" },
  blue: { start: "\x1b[1;94m", end: "\x1b[22;39m" },
  green: { start: "\x1b[1;92m", end: "\x1b[22;39m" },
  cyan: { start: "\x1b[1;96m", end: "\x1b[22;39m" },
  magenta: { start: "\x1b[1;95m", end: "\x1b[22;39m" },
  dim: { start: "\x1b[2;90m", end: "\x1b[22;39m" },
};

// ANSI CSI/OSC sequence matcher — styled lines are re-emitted as plain text
// with injected codes, so escapes must be stripped before matching
const ANSI_CHUNK = /\x1b(?:\][^\x07\x1b]*(?:\x07|\x1b\\)|\[[0-9;?]*[a-zA-Z])/g;

// full-screen / cursor-control heuristics: when present, never touch the chunk
const FULLSCREEN_MARKERS = ["\x1b[?1049", "\x1b[H", "\x1b[2J", "\x1b[3J", "\x1b[6n"];

// leading OSC sequences (prompt-time cwd reports etc.) — preserved when a
// matched line is re-emitted as colored plain text, so shell-integration
// sequences riding on a highlighted line are never swallowed
const LEADING_OSC = /^(?:\x1b\][^\x07\x1b]*(?:\x07|\x1b\\))*/;

function stripAnsi(text: string): string {
  return text.replace(ANSI_CHUNK, "");
}

interface CompiledRule {
  regex: RegExp;
  rule: HighlightRule;
}

export class Highlighter {
  private enabled = true;
  private rules: CompiledRule[] = [];
  // one decoder per stream keeps UTF-8 sequences split across chunks intact;
  // every chunk passes through decode+encode so the emitted stream and the
  // decoder state never diverge
  private decoder = new TextDecoder("utf-8");
  private encoder = new TextEncoder();

  constructor(settings?: Partial<HighlightSettings>) {
    if (settings?.enabled !== undefined) this.enabled = settings.enabled;
    this.setRules(settings?.rules ?? DEFAULT_RULES);
  }

  setEnabled(enabled: boolean): void {
    this.enabled = enabled;
  }

  setRules(rules: HighlightRule[]): void {
    this.rules = [];
    for (const rule of rules) {
      if (!rule.enabled || !rule.pattern) continue;
      try {
        this.rules.push({ rule, regex: new RegExp(rule.pattern, "gi") });
      } catch {
        // invalid pattern: skip the rule, the settings UI flags it instead
      }
    }
  }

  private applyToLine(text: string): string {
    for (const { rule, regex } of this.rules) {
      if (rule.mode === "line") {
        regex.lastIndex = 0;
        if (regex.test(text)) {
          const style = STYLES[rule.color];
          return style.start + text + style.end;
        }
        continue;
      }
      // segment mode: wrap every non-overlapping match, right to left so the
      // insertion indexes stay valid
      regex.lastIndex = 0;
      const ranges: [number, number][] = [];
      let match: RegExpExecArray | null;
      while ((match = regex.exec(text)) !== null) {
        if (match[0].length === 0) {
          regex.lastIndex += 1;
          continue;
        }
        ranges.push([match.index, match.index + match[0].length]);
      }
      for (let i = ranges.length - 1; i >= 0; i--) {
        const [start, end] = ranges[i];
        const style = STYLES[rule.color];
        text = text.slice(0, start) + style.start + text.slice(start, end) + style.end + text.slice(end);
      }
    }
    return text;
  }

  // process styles every line completed inside this chunk and passes the
  // trailing partial line through verbatim (interactive echo must never
  // wait for a newline that only the user's next keystroke will send).
  process(chunk: Uint8Array): Uint8Array {
    const text = this.decoder.decode(chunk, { stream: true });
    if (!this.enabled || this.rules.length === 0) {
      return this.encoder.encode(text);
    }
    if (FULLSCREEN_MARKERS.some((marker) => text.includes(marker))) {
      return this.encoder.encode(text);
    }
    const lastNewline = text.lastIndexOf("\n");
    if (lastNewline === -1) {
      return this.encoder.encode(text);
    }
    let out = "";
    for (const line of text.slice(0, lastNewline).split("\n")) {
      // very long lines skip styling: user-supplied regexes can backtrack
      // catastrophically and one huge line must never freeze the UI thread
      if (line.length <= 4000) {
        const plain = stripAnsi(line);
        const styled = this.applyToLine(plain);
        // a rule matched when the styled text differs: emit the colored plain
        // text (with any leading OSC kept — the follow feature listens to
        // them); otherwise keep the original bytes with their own escapes
        out += (styled !== plain ? (LEADING_OSC.exec(line)?.[0] ?? "") + styled : line) + "\n";
      } else {
        out += line + "\n";
      }
    }
    out += text.slice(lastNewline + 1);
    return this.encoder.encode(out);
  }
}
