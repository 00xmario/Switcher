// Live state: Switcher holds GET /api/state/changes open until its state
// differs from the tag this page saw last, then answers at once. The page
// waits again after every answer and reads the state when it changed, so a
// switch made on another computer, or by automatic switching, shows at once.
// The page's regular poll stays: it covers a server from before this route,
// errors, and the countdowns it repaints every minute.

export const CHANGES_PATH = '/api/state/changes';
// A server from before the route answers 404. It may be updated while the
// page stays open, so the route is tried again after a while.
export const OLD_SERVER_RETRY_MS = 5 * 60 * 1000;
export const MAX_BACKOFF_MS = 30 * 1000;

export function retryDelay(failures) {
  return Math.min(1000 * 2 ** Math.max(0, failures - 1), MAX_BACKOFF_MS);
}

// request(path, { signal }) resolves with the decoded answer or rejects with
// an error carrying the HTTP status; onChange reads the state; active says
// whether the page should wait at all (shown, and signed in).
export function createLiveState({ request, onChange, active,
  sleep = ms => new Promise(resolve => setTimeout(resolve, ms)) }) {
  let etag = '';
  let generation = 0;
  let running = false;
  let controller = null;
  let failures = 0;

  async function run(own) {
    while (own === generation && active()) {
      controller = new AbortController();
      let answer, error;
      try {
        answer = await request(`${CHANGES_PATH}?since=${encodeURIComponent(etag)}`, { signal: controller.signal });
      } catch (failure) {
        error = failure;
      }
      if (own !== generation) return;
      if (!error && (typeof answer?.etag !== 'string' || typeof answer?.changed !== 'boolean')) {
        error = new Error('Switcher sent an odd answer');
      }
      if (error) {
        if (error.status === 404) await sleep(OLD_SERVER_RETRY_MS);
        else await sleep(retryDelay(++failures));
        continue;
      }
      failures = 0;
      etag = answer.etag;
      if (answer.changed) Promise.resolve().then(onChange).catch(() => {});
    }
    if (own === generation) running = false;
  }

  // start waits unless it already does; the tag seen last is kept, so a page
  // shown again hears at once about a change made while it was hidden.
  function start() {
    if (running || !active()) return;
    running = true;
    run(++generation);
  }

  // stop ends the wait now, so a hidden page holds no request open.
  function stop() {
    generation++;
    running = false;
    controller?.abort();
    controller = null;
  }

  return { start, stop, get etag() { return etag; }, get running() { return running; } };
}
