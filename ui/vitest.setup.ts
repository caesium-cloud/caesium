import '@testing-library/jest-dom/vitest'
import { cleanup } from '@testing-library/react'
import { afterEach } from 'vitest'

function createStorageMock(): Storage {
  const store = new Map<string, string>()

  return {
    get length() {
      return store.size
    },
    clear() {
      store.clear()
    },
    getItem(key: string) {
      return store.has(key) ? store.get(key)! : null
    },
    key(index: number) {
      return Array.from(store.keys())[index] ?? null
    },
    removeItem(key: string) {
      store.delete(key)
    },
    setItem(key: string, value: string) {
      store.set(key, String(value))
    },
  }
}

if (typeof window !== 'undefined' && typeof window.localStorage?.clear !== 'function') {
  const storage = createStorageMock()
  Object.defineProperty(window, 'localStorage', {
    configurable: true,
    value: storage,
  })
}

afterEach(() => {
  cleanup()
})
