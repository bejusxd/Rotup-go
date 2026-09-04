import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { MemoryRouter } from "react-router";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { CredentialForm, SourceForm, SourcePage } from "./Sources";
import { DisksPage } from "./Disks";
import { SetupPage } from "./Setup";
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
    vi.mocked(useQuery).mockImplementation((path) => ({
      loading: false,
      path,
      refreshing: false,
      reload: vi.fn(),
      data:
        path === "/setup"
          ? {
              system: {
                ready: true,
                checked: true,
                issues: [],
                command: "",
                mediaAvailable: true,
              },
              steps: [],
              complete: false,
              nextStep: null,
              recentRunLimit: 100,
            }
          : {
              applicationStatus: "running",
              databaseStatus: "ok",
              sources: [],
              repositories: [],
              recentRuns: [],
              recentRunLimit: 100,
              lastSuccessfulBackupAt: null,
              storage: {
                disks: [],
                enumerationError: "",
                platformAvailable: true,
              },
            },
    }));
    render(
      <MemoryRouter>
        <SystemContext.Provider
          value={
            {
              transferEngine: { name: "rclone", available: true },
              backupEngine: {
                name: "restic",
                available: false,
                detail: "not found on the configured path",
              },
            } as System
          }
        >
          <DashboardPage />
        </SystemContext.Provider>
      </MemoryRouter>,
    );
    expect(screen.getByText("Some items need attention")).toBeVisible();
    expect(screen.getByText("A required tool is unavailable.")).toBeVisible();
    // The operator is told which capability is lost, not which binary.
    expect(
      screen.getByText(/Backing up to a disk needs the snapshot tool/),
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
  it("asks which disk the storage belongs to before asking for its password", () => {
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
      <MemoryRouter initialEntries={["/repositories/new"]}>
        <SystemContext.Provider
          value={{ minimumRepositoryPasswordLength: 12 } as System}
        >
          <RepositorySetupPage />
        </SystemContext.Provider>
      </MemoryRouter>,
    );
    expect(screen.getByLabelText("Backup disk")).toBeVisible();
    expect(
      screen.queryByLabelText("New repository password"),
    ).not.toBeInTheDocument();
    expect(screen.queryByRole("checkbox")).not.toBeInTheDocument();
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
    expect(
      screen.getByRole("heading", { level: 1, name: "Page not found" }),
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
  it("renders the next action the backend derived, and does not derive its own", () => {
    const nextStep = {
      id: "SOURCE_CONNECTION_TESTED",
      title: "Connection tested successfully",
      done: false,
      evidence: "There is no record of a successful connection test.",
      action: "Test the connection",
      href: "/sources/source-1",
      blocked: false,
    };
    vi.mocked(useQuery).mockImplementation((path) => ({
      loading: false,
      path,
      refreshing: false,
      reload: vi.fn(),
      data:
        path === "/setup"
          ? {
              system: {
                ready: true,
                checked: true,
                issues: [],
                command: "sudo rotupd setup",
                mediaAvailable: true,
              },
              steps: [
                {
                  id: "SOURCE_CONFIGURED",
                  title: "Source configured",
                  done: true,
                  evidence: "1 source configured: Photos.",
                  action: "Add a source",
                  href: "/sources/source-1",
                  blocked: false,
                },
                nextStep,
              ],
              complete: false,
              nextStep,
              recentRunLimit: 100,
            }
          : {
              applicationStatus: "running",
              databaseStatus: "ok",
              sources: [
                {
                  id: "source-1",
                  name: "Photos",
                  enabled: true,
                  status: "STABLE",
                },
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
    }));
    render(
      <MemoryRouter>
        <SystemContext.Provider
          value={
            {
              transferEngine: { name: "rclone", available: true },
              backupEngine: { name: "restic", available: true },
            } as System
          }
        >
          <DashboardPage />
        </SystemContext.Provider>
      </MemoryRouter>,
    );
    // The action and its destination both come from the API. The dashboard
    // used to compute this itself, which let two places disagree.
    const links = screen.getAllByRole("link", { name: /Test the connection/ });
    expect(links.length).toBeGreaterThan(0);
    links.forEach((link) =>
      expect(link).toHaveAttribute("href", "/sources/source-1"),
    );
    expect(
      screen.queryByRole("link", { name: /Continue setup/ }),
    ).not.toBeInTheDocument();
  });

  it("leads with an unprovisioned host before anything else on the dashboard", () => {
    vi.mocked(useQuery).mockImplementation((path) => ({
      loading: false,
      path,
      refreshing: false,
      reload: vi.fn(),
      data:
        path === "/setup"
          ? {
              system: {
                ready: false,
                checked: true,
                issues: [],
                command: "sudo rotupd setup",
                mediaAvailable: false,
              },
              steps: [],
              complete: false,
              nextStep: null,
              recentRunLimit: 100,
            }
          : {
              applicationStatus: "running",
              databaseStatus: "ok",
              sources: [],
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
    }));
    render(
      <MemoryRouter>
        <SystemContext.Provider
          value={
            {
              transferEngine: { name: "rclone", available: true },
              backupEngine: { name: "restic", available: true },
            } as System
          }
        >
          <DashboardPage />
        </SystemContext.Provider>
      </MemoryRouter>,
    );
    expect(
      screen.getByText("This host is not fully provisioned."),
    ).toBeVisible();
    expect(screen.getByText("sudo rotupd setup")).toBeVisible();
  });
});

describe("operator language and audit separation", () => {
  it("drops empty rule lines instead of sending them to the collector", async () => {
    vi.mocked(mutate).mockResolvedValue({ id: "source-1" });
    render(
      <MemoryRouter>
        <SourceForm />
      </MemoryRouter>,
    );
    fireEvent.change(screen.getByLabelText("Name"), {
      target: { value: "Photos" },
    });
    fireEvent.change(screen.getByLabelText("Server / host"), {
      target: { value: "nas" },
    });
    fireEvent.change(screen.getByLabelText("Share"), {
      target: { value: "photos" },
    });
    fireEvent.change(screen.getByLabelText(/Exclude rules/), {
      target: { value: ["*.tmp", "", "  ", "*.part"].join("\n") },
    });
    fireEvent.click(screen.getByRole("button", { name: "Save source" }));
    await waitFor(() => expect(mutate).toHaveBeenCalledTimes(1));
    expect(vi.mocked(mutate).mock.calls[0][1]).toMatchObject({
      excludeRules: ["*.tmp", "*.part"],
      includeRules: [],
    });
  });
  it("keeps the state version out of the normal source view but not out of the record", () => {
    vi.mocked(useQuery).mockReturnValue({
      loading: false,
      path: "/sources/source-1",
      refreshing: false,
      reload: vi.fn(),
      data: {
        source: {
          id: "source-1",
          name: "Photos",
          host: "nas",
          shareName: "photos",
          enabled: true,
          status: "STABLE",
          stateVersion: 7,
          stagingRelativePath: "photos",
          policy: "archive_then_sync",
          hasCredential: true,
          busy: false,
          lastPublishedAt: "2026-09-04T10:00:00Z",
          lastCollectionRunId: "run-1",
        },
        runs: [],
        revisionBatches: [],
        historyLimit: 100,
      },
    });
    render(
      <MemoryRouter>
        <SourcePage />
      </MemoryRouter>,
    );
    expect(screen.queryByText("Local copy version")).not.toBeInTheDocument();
    // The exact recorded version an audit needs is still on the page.
    expect(screen.getByText("7")).toBeInTheDocument();
    expect(
      screen.getByText(/Test the connection to confirm/),
    ).toBeInTheDocument();
  });
});

describe("first-run onboarding", () => {
  const baseState = {
    system: {
      ready: true,
      checked: true,
      issues: [],
      command: "sudo rotupd setup",
      mediaAvailable: true,
    },
    steps: [
      {
        id: "SOURCE_CONFIGURED",
        title: "Source configured",
        done: true,
        evidence: "1 source configured: Photos.",
        action: "Add a source",
        href: "/sources/source-1",
        blocked: false,
      },
      {
        id: "SOURCE_CONNECTION_TESTED",
        title: "Connection tested successfully",
        done: false,
        evidence: "There is no record of a successful connection test.",
        action: "Test the connection",
        href: "/sources/source-1",
        blocked: false,
      },
    ],
    complete: false,
    generatedAt: "2026-09-04T00:00:00Z",
    recentRunLimit: 100,
  };

  function renderSetup(state: unknown) {
    vi.mocked(useQuery).mockReturnValue({
      loading: false,
      path: "/setup",
      refreshing: false,
      reload: vi.fn(),
      data: state,
    });
    render(
      <MemoryRouter>
        <SystemContext.Provider value={{ devMode: false } as System}>
          <SetupPage />
        </SystemContext.Provider>
      </MemoryRouter>,
    );
  }

  it("renders the backend's step state and never recomputes it", () => {
    renderSetup({
      ...baseState,
      nextStep: baseState.steps[1],
    });
    expect(screen.getByText("Source configured")).toBeVisible();
    // The evidence the daemon recorded is shown verbatim; the page does not
    // reword or re-derive whether a step is done.
    expect(
      screen.getByText("There is no record of a successful connection test."),
    ).toBeVisible();
    const action = screen.getAllByRole("link", { name: /Test the connection/ });
    expect(action.length).toBeGreaterThan(0);
    action.forEach((link) =>
      expect(link).toHaveAttribute("href", "/sources/source-1"),
    );
  });

  it("points an unprovisioned host at the terminal and never offers to fix it", () => {
    renderSetup({
      ...baseState,
      system: {
        ready: false,
        checked: true,
        command: "sudo rotupd setup",
        mediaAvailable: false,
        issues: [
          {
            id: "helper.installed",
            title: "Mount helper",
            status: "ERROR",
            detail: "/usr/libexec/rotup-mounthelper is not installed",
            remedy: "Run `sudo rotupd setup`.",
            critical: true,
          },
        ],
      },
      steps: [
        { ...baseState.steps[0], done: false },
        {
          id: "BACKUP_DISK_REGISTERED",
          title: "Backup disk registered",
          done: false,
          evidence: "No disk is registered.",
          action: "Register a backup disk",
          href: "/disks",
          blocked: true,
        },
      ],
      nextStep: { ...baseState.steps[0], done: false },
    });

    expect(screen.getByText("Not provisioned")).toBeVisible();
    expect(screen.getByText(/is not installed/)).toBeVisible();
    expect(screen.getAllByText("sudo rotupd setup").length).toBeGreaterThan(0);
    // A step the host cannot support yet offers no action at all, rather than
    // a control that would fail.
    expect(screen.getByText("Needs system setup")).toBeVisible();
    expect(
      screen.queryByRole("link", { name: /Register a backup disk/ }),
    ).not.toBeInTheDocument();
    // Nothing on this page may attempt privileged provisioning.
    expect(mutate).not.toHaveBeenCalled();
    expect(
      screen.queryByRole("button", { name: /provision|install|sudo/i }),
    ).not.toBeInTheDocument();
  });

  it("says plainly when the daemon could not inspect its own host", () => {
    renderSetup({
      ...baseState,
      system: { ...baseState.system, checked: false, ready: true },
      nextStep: null,
    });
    expect(
      screen.getByText("This daemon could not inspect its own host."),
    ).toBeVisible();
    // An unchecked host must not be presented as provisioned.
    expect(screen.queryByText("Provisioned")).not.toBeInTheDocument();
  });

  it("reports completion only when the backend says every step is confirmed", () => {
    renderSetup({
      ...baseState,
      steps: baseState.steps.map((step) => ({ ...step, done: true })),
      complete: true,
      nextStep: null,
    });
    expect(screen.getByText("Every setup step is confirmed.")).toBeVisible();
    expect(screen.getByText("2 of 2 confirmed")).toBeVisible();
  });
});
