import { useCallback, useEffect, useState } from "react";
import { request } from "./client";

interface QueryState<T> {
  path: string;
  data?: T;
  error?: unknown;
  loading: boolean;
  refreshing: boolean;
}

export function useQuery<T>(path: string, pollMs = 0) {
  const [revision, setRevision] = useState(0);
  const [state, setState] = useState<QueryState<T>>({
    path,
    loading: true,
    refreshing: false,
  });
  useEffect(() => {
    const controller = new AbortController();
    let timer: ReturnType<typeof setTimeout> | undefined;
    async function read() {
      try {
        const data = await request<T>(path, { signal: controller.signal });
        if (!controller.signal.aborted)
          setState({ path, data, loading: false, refreshing: false });
      } catch (error) {
        // Do not leave old healthy data visible when the backend becomes unreachable.
        if (!controller.signal.aborted)
          setState({ path, error, loading: false, refreshing: false });
      }
      if (pollMs && !controller.signal.aborted)
        timer = setTimeout(read, pollMs);
    }
    // Re-reading the same resource keeps the last known values on screen, so a
    // manual refresh or a reload after an action does not blank the page and
    // throw away the operator's scroll position. Switching to a different path
    // must never show the previous resource's data.
    setState((previous) =>
      previous.path === path && previous.data !== undefined
        ? { ...previous, refreshing: true }
        : { path, loading: true, refreshing: false },
    );
    void read();
    return () => {
      controller.abort();
      clearTimeout(timer);
    };
  }, [path, pollMs, revision]);
  return {
    ...(state.path === path
      ? state
      : {
          path,
          loading: true,
          refreshing: false,
          data: undefined,
          error: undefined,
        }),
    reload: useCallback(() => setRevision((v) => v + 1), []),
  };
}
