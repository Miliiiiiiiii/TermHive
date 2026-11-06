// CodeMirror 6 wrapper for the remote file editor: syntax highlighting for
// common server-side file types (legacy stream modes), a host-theme-aligned
// look, read-only windowed mode and a Mod-s hook. Kept deliberately small —
// the editor overlay owns load/save/windowing, this module only owns the
// editing surface.
import { EditorState } from "@codemirror/state";
import { EditorView, keymap } from "@codemirror/view";
import {
  HighlightStyle,
  StreamLanguage,
  syntaxHighlighting,
} from "@codemirror/language";
import { tags as t } from "@lezer/highlight";
import { shell } from "@codemirror/legacy-modes/mode/shell";
import { python } from "@codemirror/legacy-modes/mode/python";
import { xml } from "@codemirror/legacy-modes/mode/xml";
import { yaml } from "@codemirror/legacy-modes/mode/yaml";
import { javascript, json } from "@codemirror/legacy-modes/mode/javascript";
import { sql } from "@codemirror/legacy-modes/mode/sql";
import { cpp } from "@codemirror/legacy-modes/mode/clike";
import { go } from "@codemirror/legacy-modes/mode/go";
import { rust } from "@codemirror/legacy-modes/mode/rust";
import { properties } from "@codemirror/legacy-modes/mode/properties";
import { defaultKeymap, history, historyKeymap } from "@codemirror/commands";

// One Dark-ish palette; the chrome (background, gutters, cursor) rides the
// host theme tokens so light/dark follows the host for free.
const editorTheme = EditorView.theme({
  "&": { backgroundColor: "var(--bg-panel)", color: "var(--text)", height: "100%", fontSize: "13px" },
  ".cm-content": { fontFamily: "var(--mono, monospace)", caretColor: "var(--accent)" },
  ".cm-gutters": { backgroundColor: "var(--bg-panel)", color: "var(--text-dim)", border: "none" },
  ".cm-activeLine": { backgroundColor: "var(--bg-hover)" },
  ".cm-activeLineGutter": { backgroundColor: "var(--bg-hover)" },
  ".cm-selectionBackground, &.cm-focused .cm-selectionBackground": {
    backgroundColor: "var(--accent-soft)",
  },
  ".cm-cursor": { borderLeftColor: "var(--accent)" },
});

const editorHighlights = HighlightStyle.define([
  { tag: t.keyword, color: "#c678dd" },
  { tag: t.string, color: "#98c379" },
  { tag: t.comment, color: "#7f848e", fontStyle: "italic" },
  { tag: t.number, color: "#d19a66" },
  { tag: t.atom, color: "#56b6c2" },
  { tag: t.bool, color: "#56b6c2" },
  { tag: t.variableName, color: "#e06c75" },
  { tag: t.propertyName, color: "#e5c07b" },
  { tag: t.typeName, color: "#e5c07b" },
  { tag: t.operator, color: "#56b6c2" },
]);

const MODES: Record<string, () => StreamLanguage<unknown>> = {
  sh: () => StreamLanguage.define(shell),
  bash: () => StreamLanguage.define(shell),
  zsh: () => StreamLanguage.define(shell),
  py: () => StreamLanguage.define(python),
  xml: () => StreamLanguage.define(xml),
  html: () => StreamLanguage.define(xml),
  yml: () => StreamLanguage.define(yaml),
  yaml: () => StreamLanguage.define(yaml),
  json: () => StreamLanguage.define(json),
  js: () => StreamLanguage.define(javascript),
  ts: () => StreamLanguage.define(javascript),
  sql: () => StreamLanguage.define(sql({})),
  c: () => StreamLanguage.define(cpp),
  h: () => StreamLanguage.define(cpp),
  cpp: () => StreamLanguage.define(cpp),
  hpp: () => StreamLanguage.define(cpp),
  java: () => StreamLanguage.define(cpp),
  go: () => StreamLanguage.define(go),
  rs: () => StreamLanguage.define(rust),
  conf: () => StreamLanguage.define(properties),
  ini: () => StreamLanguage.define(properties),
  properties: () => StreamLanguage.define(properties),
};

function languageFor(path: string) {
  const extension = path.includes(".") ? path.split(".").pop()!.toLowerCase() : "";
  return MODES[extension]?.() ?? null;
}

export interface CodeEditor {
  setDoc(text: string, readOnly: boolean): void;
  getValue(): string;
  scrollToEnd(): void;
  setSaveHandler(handler: () => void): void;
  dispose(): void;
}

export function createCodeEditor(container: HTMLElement, path: string): CodeEditor {
  let saveHandler: () => void = () => undefined;
  const language = languageFor(path);
  const view = new EditorView({
    parent: container,
    state: EditorState.create({
      doc: "",
      extensions: [
        history(),
        keymap.of([...defaultKeymap, ...historyKeymap]),
        language ? syntaxHighlighting(editorHighlights, { fallback: true }) : [],
        language ?? [],
        editorTheme,
        EditorState.readOnly.of(true),
        EditorView.editable.of(false),
        keymap.of([
          {
            key: "Mod-s",
            preventDefault: true,
            run: () => {
              saveHandler();
              return true;
            },
          },
        ]),
      ],
    }),
  });

  return {
    setDoc(text: string, readOnly: boolean) {
      view.setState(
        EditorState.create({
          doc: text,
          extensions: [
            history(),
            keymap.of([...defaultKeymap, ...historyKeymap]),
            language ? syntaxHighlighting(editorHighlights, { fallback: true }) : [],
            language ?? [],
            editorTheme,
            EditorState.readOnly.of(readOnly),
            EditorView.editable.of(!readOnly),
            keymap.of([
              {
                key: "Mod-s",
                preventDefault: true,
                run: () => {
                  saveHandler();
                  return true;
                },
              },
            ]),
          ],
        }),
      );
    },
    getValue() {
      return view.state.doc.toString();
    },
    scrollToEnd() {
      const lastLine = view.state.doc.line(view.state.doc.lines);
      view.dispatch({
        effects: EditorView.scrollIntoView(lastLine.to, { y: "end" }),
      });
    },
    setSaveHandler(handler: () => void) {
      saveHandler = handler;
    },
    dispose() {
      view.destroy();
    },
  };
}
