import { EditorView } from "@codemirror/view";
import { HighlightStyle, syntaxHighlighting } from "@codemirror/language";
import { tags } from "@lezer/highlight";

// Simple custom theme for the editor to match our brand
export const yamlTheme = EditorView.theme({
  "&": {
    backgroundColor: "hsl(var(--midnight)) !important",
    color: "hsl(var(--text-1))",
    fontSize: "13px",
    fontFamily: "var(--font-sans)",
  },
  ".cm-content": {
    caretColor: "hsl(var(--cyan-glow))",
  },
  "&.cm-focused .cm-cursor": {
    borderLeftColor: "hsl(var(--cyan-glow))",
  },
  ".cm-gutters": {
    backgroundColor: "hsl(var(--midnight))",
    color: "hsl(var(--text-3))",
    borderRight: "1px solid hsl(var(--graphite))",
  },
  ".cm-activeLineGutter": {
    backgroundColor: "transparent",
    color: "hsl(var(--cyan-glow))",
  },
  ".cm-activeLine": {
    backgroundColor: "hsl(var(--obsidian))",
  },
}, { dark: true });
export const yamlHighlight = syntaxHighlighting(HighlightStyle.define([
  { tag: tags.propertyName, color: "hsl(var(--text-1))" },
  { tag: tags.string, color: "hsl(var(--text-2))" },
  { tag: [tags.number, tags.bool, tags.null], color: "hsl(var(--gold))" },
  { tag: tags.comment, color: "hsl(var(--text-3))", fontStyle: "italic" },
]));
