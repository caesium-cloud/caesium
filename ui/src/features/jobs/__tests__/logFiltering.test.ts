import { describe, expect, it } from "vitest"
import { buildLogFilterResult, splitLogLines, stripAnsi } from "../logFiltering"

describe("logFiltering", () => {
  it("preserves full output when no search term is provided", () => {
    const rawLog = "\u001b[32mready\u001b[0m\nplain line\n"
    const result = buildLogFilterResult(rawLog, "", false)

    expect(result.renderedLog).toBe(rawLog)
    expect(result.renderedText).toBe("ready\nplain line")
    expect(result.visibleLines).toBe(2)
    expect(result.totalLines).toBe(2)
  })

  it("filters rows by the visible text while keeping ANSI content", () => {
    const rawLog = "\u001b[31mError: broken\u001b[0m\nok\n{\"level\":\"error\"}\n"
    const result = buildLogFilterResult(rawLog, "error", false)

    expect(result.visibleLines).toBe(2)
    expect(result.totalLines).toBe(3)
    expect(result.renderedText).toBe("Error: broken\n{\"level\":\"error\"}")
    expect(result.renderedLog).toContain("\u001b[31mError: broken\u001b[0m")
    expect(result.renderedLog).not.toContain("\nok\n")
  })

  it("supports case-sensitive filtering", () => {
    const rawLog = "Error\nerror\n"

    expect(buildLogFilterResult(rawLog, "Error", true).visibleLines).toBe(1)
    expect(buildLogFilterResult(rawLog, "Error", false).visibleLines).toBe(2)
  })

  it("splits CRLF logs and strips ANSI escapes", () => {
    const lines = splitLogLines("\u001b[34mblue\u001b[0m\r\nplain\r\n")

    expect(lines).toEqual([
      { plain: "blue", raw: "\u001b[34mblue\u001b[0m" },
      { plain: "plain", raw: "plain" },
    ])
    expect(stripAnsi("\u001b[33mwarn\u001b[0m")).toBe("warn")
  })
})
