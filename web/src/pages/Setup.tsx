import { Link } from "react-router";
import { useQuery } from "../api/hooks";
import type { SetupState, SetupStep, SetupSystem } from "../api/types";
import {
  Alert,
  ErrorAlert,
  LoadingState,
  PageHeader,
  TechnicalDetails,
} from "../components";

// Two kinds of setup meet on this page and they have different remedies.
// System setup is provisioning the host -- the privileged helper, its sudo
// policy, the runtime directory -- and is performed at a terminal with
// `rotupd setup`. Application setup is configuring a source and running the
// first backup, and is performed here. The page never offers to do the first
// kind: a browser request must not be able to change a sudo policy.
export function SetupPage() {
  const query = useQuery<SetupState>("/setup", 10000);
  if (query.loading) return <LoadingState />;
  if (query.error)
    return (
      <>
        <PageHeader title="Setup" />
        <ErrorAlert error={query.error} />
        <button onClick={query.reload}>Try again</button>
      </>
    );
  const state = query.data!;
  return (
    <>
      <PageHeader
        title="Setup"
        description="What ROTUP has actually recorded, and the one thing to do next."
      >
        <button
          className="secondary"
          disabled={query.refreshing}
          onClick={query.reload}
        >
          {query.refreshing ? "Refreshing…" : "Refresh"}
        </button>
        {state.nextStep && !state.nextStep.blocked && (
          <Link className="button" to={state.nextStep.href}>
            {state.nextStep.action}
          </Link>
        )}
      </PageHeader>

      <SystemSection system={state.system} />

      <section className="panel">
        <div className="section-heading">
          <h2>Application setup</h2>
          <span className="muted small">
            {state.steps.filter((s) => s.done).length} of {state.steps.length}{" "}
            confirmed
          </span>
        </div>
        <p className="muted">
          Performed here, in this interface. Each step is confirmed from what
          ROTUP recorded, not from the fact that you visited a page.
        </p>
        <ol className="setup-steps">
          {state.steps.map((step) => (
            <StepRow
              key={step.id}
              step={step}
              current={state.nextStep?.id === step.id}
            />
          ))}
        </ol>
        <p className="muted small">
          Connection evidence comes from the latest {state.recentRunLimit} runs.
          A step this page cannot confirm is not proof it never happened.
        </p>
      </section>

      {state.complete && (
        <Alert tone="info">
          <strong>Every setup step is confirmed.</strong>
          <p>
            A completed backup is recorded. Keep rotating disks and repeat the
            backup; ROTUP does not schedule anything on its own.
          </p>
          <Link to="/">Go to the overview →</Link>
        </Alert>
      )}
    </>
  );
}

function SystemSection({ system }: { system: SetupSystem }) {
  if (!system.checked)
    return (
      <section className="panel">
        <h2>System setup</h2>
        <Alert>
          <strong>This daemon could not inspect its own host.</strong>
          <p>
            Run <code>rotupd doctor</code> on the machine to check the mount
            helper, its sudo policy and the runtime directory.
          </p>
        </Alert>
      </section>
    );
  const blocking = system.issues.filter((issue) => issue.critical);
  return (
    <section className="panel">
      <div className="section-heading">
        <h2>System setup</h2>
        <span className={`badge ${system.ready ? "good" : "bad"}`}>
          <span aria-hidden="true" className="dot" />
          {system.ready ? "Provisioned" : "Not provisioned"}
        </span>
      </div>
      <p className="muted">
        Performed once at a terminal, not from this page. ROTUP never changes a
        sudo policy or a system directory in response to a browser request.
      </p>

      {system.ready && !system.issues.length && (
        <p>
          The mount helper, its sudo policy, the runtime directory and the data
          locations are all in place.
        </p>
      )}

      {!!blocking.length && (
        <Alert tone="danger">
          <strong>
            {blocking.length === 1
              ? "One part of this host is not provisioned."
              : `${blocking.length} parts of this host are not provisioned.`}
          </strong>
          <p>
            Run <code>{system.command}</code> on the machine, then refresh this
            page.
          </p>
        </Alert>
      )}

      {!system.mediaAvailable && (
        <Alert>
          <strong>Removable-media management is unavailable.</strong>
          <p>
            Backup disks cannot be registered or used until the mount helper is
            configured. Collecting a source into the local copy still works.
          </p>
        </Alert>
      )}

      {!!system.issues.length && (
        <ul className="issue-list">
          {system.issues.map((issue) => (
            <li key={issue.id}>
              <span className={issue.critical ? "step" : "step done"}>
                {issue.critical ? "×" : "!"}
              </span>
              <div>
                <strong>{issue.title}</strong>
                <p className="muted small">{issue.detail}</p>
                {issue.remedy && <p className="small">{issue.remedy}</p>}
              </div>
            </li>
          ))}
        </ul>
      )}

      <TechnicalDetails>
        <p>
          These findings come from the same read-only inspection as{" "}
          <code>rotupd doctor</code>. Run <code>rotupd doctor --active</code> on
          the host to also probe tool versions and the privileged helper
          invocation, or <code>rotupd doctor --json</code> for a support bundle.
        </p>
      </TechnicalDetails>
    </section>
  );
}

function StepRow({ step, current }: { step: SetupStep; current: boolean }) {
  return (
    <li className={current ? "setup-step current" : "setup-step"}>
      <span
        aria-label={step.done ? "Confirmed" : "Not confirmed"}
        className={step.done ? "step done" : "step"}
      >
        {step.done ? "✓" : "○"}
      </span>
      <div className="setup-step-body">
        <strong>{step.title}</strong>
        <p className="muted small">{step.evidence}</p>
      </div>
      {step.done ? (
        <Link to={step.href}>View →</Link>
      ) : step.blocked ? (
        <span className="muted small">Needs system setup</span>
      ) : (
        <Link className={current ? "button" : ""} to={step.href}>
          {current ? step.action : `${step.action} →`}
        </Link>
      )}
    </li>
  );
}
