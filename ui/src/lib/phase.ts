/** All live instruments share UTC phase, including when a throttled tab returns. */
export function syncPhase(now = Date.now()) {
  for (const seconds of [1, 2, 6, 22, 30, 38]) {
    document.documentElement.style.setProperty(seconds === 1 ? "--cs-phase" : `--cs-phase-${seconds}`, `${-(now % (seconds * 1000))}ms`);
  }
}
