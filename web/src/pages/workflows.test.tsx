import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { CredentialForm, SourceForm } from "./Sources";
import { RunPage } from "./Runs";
import { RepositoryPage, RepositorySetupPage } from "./Repositories";
import { DebugPage } from "./Debug";
import { SystemContext } from "../components";
import { mutate } from "../api/client";
import { useQuery } from "../api/hooks";
import type { RunDetail, Source, System } from "../api/types";

vi.mock("../api/client", async (importOriginal) => ({
  ...(await importOriginal<typeof import("../api/client")>()),
  mutate: vi.fn(),
}));
vi.mock("../api/hooks", () => ({ useQuery: vi.fn() }));

beforeEach(() => vi.clearAllMocks());

describe("management forms", () => {
  it("saves source settings without including runtime state or credential references", async () => {
    vi.mocked(mutate).mockResolvedValue({ id: "source-1" });
    const source = {
      id: "source-1",
      name: "Photos",
      host: "nas",
      shareName: "photos",
      username: "reader",
      subdirectory: "",
      stagingRelativePath: "photos",
      policy: "archive_then_sync",
      enabled: true,
      includeRules: [],
      excludeRules: [],
      stateVersion: 12,
      hasCredential: true,
    } as unknown as Source;
    render(
      <MemoryRouter>
        <SourceForm initial={source} />
      </MemoryRouter>,
    );
    fireEvent.click(screen.getByRole("button", { name: "Save source" }));
    await waitFor(() => expect(mutate).toHaveBeenCalledTimes(1));
    const [, body, method] = vi.mocked(mutate).mock.calls[0];
    expect(method).toBe("PUT");
    expect(body).toMatchObject({
      name: "Photos",
      enabled: true,
      policy: "archive_then_sync",
    });
    expect(body).not.toHaveProperty("stateVersion");
    expect(body).not.toHaveProperty("hasCredential");
    expect(body).not.toHaveProperty("password");
  });
  it("clears the password input even after failure and never retries it", async () => {
    vi.mocked(mutate).mockRejectedValue(new Error("test failure"));
    render(
      <CredentialForm
        source={{ id: "source-1", hasCredential: false } as Source}
        onSaved={vi.fn()}
      />,
    );
    fireEvent.change(screen.getByLabelText("New password"), {
      target: { value: "test-only-secret" },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save password" }));
    await waitFor(() =>
      expect(screen.getByLabelText("New password")).toHaveValue(""),
    );
    expect(mutate).toHaveBeenCalledTimes(1);
    expect(mutate).toHaveBeenCalledWith("/sources/source-1/credential", {
      action: "replace",
      password: "test-only-secret",
    });
    expect(screen.queryByText("test-only-secret")).not.toBeInTheDocument();
  });
});

describe("backup success presentation", () => {
  it("does not claim durable success from a candidate snapshot", () => {
    const data = {
      run: {
        id: "run-1",
        kind: "BACKUP_LATEST",
        terminal: true,
        status: "FAILED",
        error: null,
        requestedAt: null,
        startedAt: null,
        finishedAt: null,
        durationMs: 0,
        summary: "",
      },
      source: null,
      collection: null,
      revisionBatches: [],
      backup: {
        repositoryId: "repo-1",
        candidateSnapshotId: "candidate-only",
        durableSuccessAt: null,
      },
    } as unknown as RunDetail;
    vi.mocked(useQuery).mockReturnValue({
      data,
      loading: false,
      reload: vi.fn(),
      path: "",
    });
    render(
      <MemoryRouter>
        <RunPage />
      </MemoryRouter>,
    );
    expect(
      screen.getByText(/No durable backup success is recorded/),
    ).toBeVisible();
    expect(screen.getByText(/candidate-only/)).toBeInTheDocument();
  });
});

describe("backend-owned storage safety", () => {
  it("uses backend eligibility and does not start anything when viewing storage", () => {
    vi.mocked(useQuery).mockReturnValue({
      loading: false,
      path: "",
      reload: vi.fn(),
      data: {
        repository: {
          id: "repo-1",
          diskId: "disk-1",
          diskName: "Disk A",
          health: "HEALTHY",
          connected: true,
          established: true,
          hasPassword: true,
        },
        sources: [
          {
            source: { id: "source-1", name: "Photos" },
            eligible: false,
            reason: "staging recovery required",
          },
        ],
        history: [],
        historyLimit: 100,
        lastVerification: null,
      },
    });
    render(
      <MemoryRouter>
        <RepositoryPage />
      </MemoryRouter>,
    );
    expect(screen.getByRole("checkbox")).toBeDisabled();
    expect(screen.getByText("staging recovery required")).toBeVisible();
    expect(screen.getByRole("button", { name: "Back up now" })).toBeDisabled();
    expect(mutate).not.toHaveBeenCalled();
  });
  it("requires an explicit offline-password acknowledgement and hides setup for established storage", () => {
    vi.mocked(useQuery).mockImplementation((path) => ({
      loading: false,
      path,
      reload: vi.fn(),
      data:
        path === "/disks"
          ? {
              disks: [
                {
                  id: "disk-1",
                  name: "Disk A",
                  registered: true,
                  observed: true,
                },
              ],
              enumerationError: "",
            }
          : { repositories: [] },
    }));
    render(
      <MemoryRouter initialEntries={["/repositories/new?disk=disk-1"]}>
        <SystemContext.Provider
          value={{ minimumRepositoryPasswordLength: 12 } as System}
        >
          <RepositorySetupPage />
        </SystemContext.Provider>
      </MemoryRouter>,
    );
    expect(screen.getByRole("checkbox")).not.toBeChecked();
    expect(screen.getByRole("checkbox")).toBeRequired();
    expect(screen.getByLabelText("New repository password")).toHaveAttribute(
      "minlength",
      "12",
    );
    expect(mutate).not.toHaveBeenCalled();
  });
  it("does not fetch debug data when development mode is disabled", () => {
    render(
      <SystemContext.Provider value={{ devMode: false } as System}>
        <DebugPage />
      </SystemContext.Provider>,
    );
    expect(
      screen.getByText("Development diagnostics are disabled."),
    ).toBeVisible();
    expect(useQuery).not.toHaveBeenCalled();
  });
});
