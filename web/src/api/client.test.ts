import { afterEach, describe, expect, it, vi } from "vitest";
import { loadSystem, mutate, RequestError } from "./client";

afterEach(() => vi.unstubAllGlobals());
describe("same-origin API client", () => {
  it("deduplicates bootstrap and submits the matching CSRF header once", async () => {
    const fetch = vi
      .fn()
      .mockResolvedValueOnce(Response.json({ csrfToken: "test-csrf" }))
      .mockResolvedValueOnce(
        Response.json({ runId: "run-1" }, { status: 202 }),
      );
    vi.stubGlobal("fetch", fetch);
    await Promise.all([loadSystem(), loadSystem()]);
    await mutate("/sources/source-1/collect");
    expect(fetch).toHaveBeenCalledTimes(2);
    expect(fetch.mock.calls[1][1]).toMatchObject({
      credentials: "same-origin",
      method: "POST",
      body: "{}",
      headers: {
        "X-CSRF-Token": "test-csrf",
        "Content-Type": "application/json",
      },
    });
  });
  it("keeps the structured backend refusal and does not retry mutations", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockResolvedValue(
        Response.json(
          {
            error: {
              code: "ambiguous_identity",
              class: "INTEGRITY",
              message: "Duplicate UUID.",
              userMessage: "The disk cannot be used safely.",
            },
          },
          { status: 409 },
        ),
      ),
    );
    await expect(mutate("/disks/disk-1/mount")).rejects.toMatchObject({
      detail: { code: "ambiguous_identity", class: "INTEGRITY" },
      status: 409,
    });
    expect(fetch).toHaveBeenCalledTimes(1);
  });
  it("warns that a lost response is not proof an operation never started", async () => {
    vi.stubGlobal(
      "fetch",
      vi.fn().mockRejectedValue(new TypeError("Failed to fetch")),
    );
    await expect(mutate("/repositories/repo-1/backup")).rejects.toBeInstanceOf(
      RequestError,
    );
    expect(fetch).toHaveBeenCalledTimes(1);
  });
});
