function normalizeSearchText(value: string) {
  return value.trim().toLowerCase()
}

function searchWords(value: string) {
  return normalizeSearchText(value).split(/[^a-z0-9]+/).filter(Boolean)
}

function fieldScore(field: string, term: string) {
  const normalized = normalizeSearchText(field)
  if (!normalized) return 0
  if (normalized === term) return 1
  if (normalized.startsWith(term)) return 0.9
  if (searchWords(normalized).some((word) => word.startsWith(term))) return 0.8
  if (normalized.includes(term)) return 0.75
  return 0
}

export function commandPaletteFilter(value: string, search: string, keywords?: string[]) {
  const terms = searchWords(search)
  if (terms.length === 0) return 1

  const fields = [value, ...(keywords ?? [])]
  let score = 0
  for (const term of terms) {
    const termScore = Math.max(...fields.map((field) => fieldScore(field, term)))
    if (termScore === 0) return 0
    score += termScore
  }
  return score / terms.length
}
