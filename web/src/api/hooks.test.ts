import { act, renderHook, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { useQuery } from "./hooks";

afterEach(() => vi.unstubAllGlobals());

describe("read hook", () => {
  it("keeps the last values on screen while a manual refresh is in flight", async () => {
    let release: (value: Response) => void = () => {};
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(Response.json({ runs: ["first"] }))
      .mockImplementationOnce(
        () => new Promise<Response>((resolve) => (release = resolve)),
      );
    vi.stubGlobal("fetch", fetch);

    const { result } = renderHook(() => useQuery<{ runs: string[] }>("/runs"));
    await waitFor(() => expect(result.current.data).toBeDefined());

    act(() => result.current.reload());
    // A refresh must not blank the page: the operator keeps the previous
    // answer and only learns that a newer one is on its way.
    expect(result.current.loading).toBe(false);
    expect(result.current.refreshing).toBe(true);
    expect(result.current.data).toEqual({ runs: ["first"] });

    await act(async () => {
      release(Response.json({ runs: ["second"] }));
    });
    await waitFor(() =>
      expect(result.current.data).toEqual({ runs: ["second"] }),
    );
    expect(result.current.refreshing).toBe(false);
  });

  it("never shows one resource's data under another resource's path", async () => {
    vi.stubGlobal(
      "fetch",
      vi
        .fn()
        .mockResolvedValueOnce(Response.json({ id: "source-1" }))
        .mockImplementationOnce(() => new Promise<Response>(() => {})),
    );
    const { result, rerender } = renderHook(({ path }) => useQuery(path), {
      initialProps: { path: "/sources/source-1" },
    });
    await waitFor(() => expect(result.current.data).toBeDefined());
    rerender({ path: "/sources/source-2" });
    expect(result.current.loading).toBe(true);
    expect(result.current.data).toBeUndefined();
  });
});
