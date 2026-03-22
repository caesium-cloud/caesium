const escapeChar = "\u001b"
const ansiEscapePattern = new RegExp(`${escapeChar}(?:[@-Z\\\\-_]|\\[[0-?]*[ -/]*[@-~])`, "g")
const ansiReset = "\x1b[0m"

interface LogLine {
  plain: string
  raw: string
}

export interface LogFilterResult {
  renderedLog: string
  renderedText: string
  totalLines: number
  visibleLines: number
}

export function buildLogFilterResult(rawLog: string, searchTerm: string, caseSensitive: boolean): LogFilterResult {
  const lines = splitLogLines(rawLog)
  const totalLines = lines.length

  if (!searchTerm) {
    return {
      renderedLog: rawLog,
      renderedText: lines.map((line) => line.plain).join("\n"),
      totalLines,
      visibleLines: totalLines,
    }
  }

  const normalizedNeedle = normalizeSearchText(searchTerm, caseSensitive)
  const matchingLines = lines.filter((line) =>
    normalizeSearchText(line.plain, caseSensitive).includes(normalizedNeedle),
  )

  return {
    renderedLog: matchingLines.map((line) => `${ansiReset}${line.raw}${ansiReset}`).join("\n"),
    renderedText: matchingLines.map((line) => line.plain).join("\n"),
    totalLines,
    visibleLines: matchingLines.length,
  }
}

export function splitLogLines(rawLog: string): LogLine[] {
  if (!rawLog) {
    return []
  }

  const normalizedLog = rawLog.replace(/\r\n/g, "\n").replace(/\r/g, "\n")
  const segments = normalizedLog.split("\n")

  if (segments.length > 1 && segments[segments.length - 1] === "") {
    segments.pop()
  }

  return segments.map((segment) => ({
    plain: stripAnsi(segment),
    raw: segment,
  }))
}

export function stripAnsi(value: string): string {
  return value.replace(ansiEscapePattern, "")
}

function normalizeSearchText(value: string, caseSensitive: boolean) {
  return caseSensitive ? value : value.toLocaleLowerCase()
}
