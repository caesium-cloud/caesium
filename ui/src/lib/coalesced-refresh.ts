/** Batch event bursts without cancelling a slow refresh or losing its trailing work. */
export function coalescedRefresh(refresh: () => Promise<unknown>, delay = 250) {
  let timer: ReturnType<typeof setTimeout> | undefined;
  let inFlight = false;
  let dirty = false;
  let disposed = false;

  const schedule = () => {
    if (disposed || inFlight || timer !== undefined) return;
    timer = setTimeout(async () => {
      timer = undefined;
      if (disposed) return;
      inFlight = true;
      dirty = false;
      try {
        await refresh();
      } catch {
        // Query observers own error/retry presentation. Keep the scheduler usable.
      } finally {
        inFlight = false;
        if (dirty) schedule();
      }
    }, delay);
  };

  return {
    request() { dirty = true; schedule(); },
    dispose() { disposed = true; clearTimeout(timer); },
  };
}
