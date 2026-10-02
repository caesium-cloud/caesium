import { describe, expect, it } from "vitest";
import { formatCommandForDisplay as formatCommand } from "../utils";

describe("command display", () => {
  it("decodes escaped operators while retaining argument boundaries", () => {
    expect(formatCommand('["sh","-c","echo \'hello\' \\u0026\\u0026 echo $HOME"]')).toBe('sh -c "echo \'hello\' && echo $HOME"');
    expect(formatCommand(JSON.stringify(["tool", "", "two words", 'a"b']))).toBe(`tool "" "two words" ${JSON.stringify('a"b')}`);
  });
  it("keeps plain, malformed, and non-argv representations verbatim", () => {
    for (const raw of ["echo hello", "[bad", '{"command":"true"}', '["x",1]']) expect(formatCommand(raw)).toBe(raw);
  });
});
