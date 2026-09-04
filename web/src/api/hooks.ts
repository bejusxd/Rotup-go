import { useCallback, useEffect, useState } from 'react'
import { request } from './client'

export function useQuery<T>(path: string, pollMs = 0) {
  const [revision, setRevision] = useState(0)
  const [state, setState] = useState<{ path: string; data?: T; error?: unknown; loading: boolean }>({ path, loading: true })
  useEffect(() => {
    const controller = new AbortController()
    let timer: ReturnType<typeof setTimeout> | undefined
    async function read() {
      try {
        const data = await request<T>(path, { signal: controller.signal })
        if (!controller.signal.aborted) setState({ path, data, loading: false })
      } catch (error) {
        // Do not leave old healthy data visible when the backend becomes unreachable.
        if (!controller.signal.aborted) setState({ path, error, loading: false })
      }
      if (pollMs && !controller.signal.aborted) timer = setTimeout(read, pollMs)
    }
    setState({ path, loading: true })
    void read()
    return () => { controller.abort(); clearTimeout(timer) }
  }, [path, pollMs, revision])
  return { ...(state.path === path ? state : { loading: true, data: undefined, error: undefined }), reload: useCallback(() => setRevision(v => v + 1), []) }
}
