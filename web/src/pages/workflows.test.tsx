import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { CredentialForm, SourceForm } from "./Sources";
import { DisksPage } from "./Disks";
import { RunPage } from "./Runs";
import { RepositoryPage, RepositorySetupPage } from "./Repositories";
import { DebugPage } from "./Debug";
import { DashboardPage } from "./Dashboard";
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
      refreshing: false,
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
  it("surfaces missing backup tools without claiming a completed backup", () => {
    vi.mocked(useQuery).mockReturnValue({
      loading: false,
      path: "",
      refreshing: false,
      reload: vi.fn(),
      data: {
        applicationStatus: "running",
        databaseStatus: "ok",
        sources: [],
        repositories: [],
        recentRuns: [],
        recentRunLimit: 100,
        lastSuccessfulBackupAt: null,
        storage: { disks: [], enumerationError: "", platformAvailable: true },
      },
    });
    render(
      <MemoryRouter>
        <SystemContext.Provider
          value={
            {
              transferEngine: { available: true },
              backupEngine: { available: false },
            } as System
          }
        >
          <DashboardPage />
        </SystemContext.Provider>
      </MemoryRouter>,
    );
    expect(screen.getByText("Some items need attention")).toBeVisible();
    expect(
      screen.getByText(/One or more backup tools are unavailable/),
    ).toBeVisible();
    expect(screen.getByText("Not yet completed")).toBeVisible();
  });
  it("uses backend eligibility and does not start anything when viewing storage", () => {
    vi.mocked(useQuery).mockReturnValue({
      loading: false,
      path: "",
      refreshing: false,
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
      refreshing: false,
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

describe("disk registration guidance", () => {
  it("explains every refusal in words and keeps the raw code in technical details", () => {
    vi.mocked(useQuery).mockReturnValue({
      loading: false,
      path: "/disks",
      refreshing: false,
      reload: vi.fn(),
      data: {
        platformAvailable: true,
        platformReason: "",
        enumerationError: "",
        generatedAt: null,
        disks: [
          {
            id: "",
            name: "",
            registered: false,
            filesystemUuid: "",
            filesystemType: "ext4",
            sizeBytes: 374251520,
            supported: true,
            ambiguous: false,
            errorCode: "",
          },
          {
            id: "",
            name: "",
            registered: false,
            filesystemUuid: "6a9041f4",
            filesystemType: "swap",
            sizeBytes: 3221229568,
            supported: false,
            ambiguous: false,
            errorCode: "",
          },
        ],
      },
    });
    render(
      <MemoryRouter>
        <DisksPage />
      </MemoryRouter>,
    );
    expect(screen.getByText(/has no UUID/)).toBeVisible();
    expect(screen.getByText(/cannot use swap/)).toBeVisible();
    // A refused filesystem never offers a registration control...
    expect(
      screen.queryByRole("button", { name: "Register disk" }),
    ).not.toBeInTheDocument();
    // ...but the identity an audit would quote is still reachable.
    expect(screen.getByText("missing")).toBeInTheDocument();
    expect(screen.getByText("6a9041f4")).toBeInTheDocument();
    expect(screen.getByText("374 MB")).toBeVisible();
  });
});

describe("setup guidance", () => {
  it("names the next action and points it at the page that performs it", () => {
    vi.mocked(useQuery).mockReturnValue({
      loading: false,
      path: "/dashboard",
      refreshing: false,
      reload: vi.fn(),
      data: {
        applicationStatus: "running",
        databaseStatus: "ok",
        sources: [
          { id: "source-1", name: "Photos", enabled: true, status: "STABLE" },
        ],
        repositories: [],
        recentRuns: [],
        recentRunLimit: 100,
        lastSuccessfulBackupAt: null,
        storage: {
          disks: [],
          enumerationError: "",
          platformAvailable: true,
          platformReason: "",
        },
      },
    });
    render(
      <MemoryRouter>
        <SystemContext.Provider
          value={
            {
              transferEngine: { available: true },
              backupEngine: { available: true },
            } as System
          }
        >
          <DashboardPage />
        </SystemContext.Provider>
      </MemoryRouter>,
    );
    // The source exists but has never been tested, so that is the next step
    // and it belongs on that source's own page, not on a list.
    const next = screen.getAllByRole("link", { name: /Test the connection/ });
    expect(next.length).toBeGreaterThan(0);
    next.forEach((link) =>
      expect(link).toHaveAttribute("href", "/sources/source-1"),
    );
    expect(
      screen.queryByRole("link", { name: /Continue setup/ }),
    ).not.toBeInTheDocument();
  });
});
