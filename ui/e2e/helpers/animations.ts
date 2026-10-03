import type { Page } from "@playwright/test";

/** Scan settled UI without disabling motion or excusing contrast violations. */
export async function waitForFiniteAnimations(page: Page, timeout = 5_000): Promise<void> {
  await page.waitForFunction(() => document.getAnimations().every((animation) => {
    // Recent fleet marks slide on the shared clock; their named transform
    // transition is live motion, just like an infinite oscillator. Other
    // finite transitions and injected fades must still settle.
    if (animation instanceof CSSTransition && animation.transitionProperty === "transform" && (animation.effect as KeyframeEffect).target instanceof Element && ((animation.effect as KeyframeEffect).target as Element).classList.contains("cs-time-mark")) return true;
    const endTime = animation.effect?.getComputedTiming().endTime;
    // Infinite spinners are intentionally ongoing; finite entry/exit effects
    // must finish. Re-query every frame so cancellation/replacement cannot
    // turn completion of an obsolete animation into readiness.
    if (endTime === undefined || endTime === Infinity) return true;
    return !animation.pending && (animation.playState === "finished" || animation.playState === "idle");
  }), undefined, { timeout });
}
