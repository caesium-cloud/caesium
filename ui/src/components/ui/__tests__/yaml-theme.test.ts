import { EditorState } from "@codemirror/state";
import { EditorView } from "@codemirror/view";
import { expect, it } from "vitest";
import { yamlThemes } from "../yaml-theme";

it.each(["dark", "light"] as const)("sets CodeMirror's base styles to the %s palette", theme => {
  const state = EditorState.create({ extensions: [yamlThemes[theme]] });
  expect(state.facet(EditorView.darkTheme)).toBe(theme === "dark");
});
