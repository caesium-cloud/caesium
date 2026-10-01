/** Align each CSS instrument's own start time, including late insertions. */
function align(animations: Animation[], now: number) {
  const timeline = document.timeline?.currentTime;
  if (typeof timeline !== "number") return;
  for (const animation of animations) {
    if (!("animationName" in animation) || !String(animation.animationName).startsWith("cs-")) continue;
    const duration = animation.effect?.getTiming().duration;
    if (typeof duration !== "number" || duration <= 0 || !Number.isFinite(duration)) continue;
    animation.startTime = timeline - now % duration;
  }
}

export function syncPhase(now = Date.now()) {
  align(document.getAnimations?.() ?? [], now);
}

export function observePhase() {
  const inserted = (event: AnimationEvent) => {
    if (event.target instanceof Element) align(event.target.getAnimations?.() ?? [], Date.now());
  };
  const visible = () => { if (document.visibilityState === "visible") syncPhase(); };
  document.addEventListener("animationstart", inserted, true);
  document.addEventListener("visibilitychange", visible);
  syncPhase();
  return () => {
    document.removeEventListener("animationstart", inserted, true);
    document.removeEventListener("visibilitychange", visible);
  };
}
