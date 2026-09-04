package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestDispatchRunsTheDaemonWhenNoSubcommandIsGiven(t *testing.T) {
	// The systemd unit's ExecStart is a bare `rotupd`, so an empty argument
	// list must fall through to the daemon rather than print help.
	if _, handled := dispatch(nil, &strings.Builder{}, &strings.Builder{}); handled {
		t.Fatal("an empty argument list must run the daemon")
	}
}

func TestDispatchRejectsAnUnknownCommandWithUsage(t *testing.T) {
	var stdout, stderr strings.Builder
	code, handled := dispatch([]string{"instal"}, &stdout, &stderr)
	if !handled || code == 0 {
		t.Fatalf("an unknown command must be handled and fail: handled=%v code=%d", handled, code)
	}
	// A typo must never silently start the daemon, and must say what is valid.
	for _, want := range []string{"unknown command", "rotupd setup", "rotupd doctor"} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("usage missing %q:\n%s", want, stderr.String())
		}
	}
}

func TestDoctorIsReadOnlyAndEmitsStableJSON(t *testing.T) {
	var stdout, stderr strings.Builder
	// Doctor runs against the real host here on purpose: it must be safe to
	// run anywhere, including a developer workstation with nothing installed.
	code := runDoctor([]string{"--json"}, &stdout, &stderr)
	if code != 0 && code != 1 {
		t.Fatalf("doctor exited %d; only 0 and 1 are contract values (stderr %q)", code, stderr.String())
	}

	var document struct {
		Kind    string `json:"kind"`
		Ready   bool   `json:"ready"`
		Changed bool   `json:"changed"`
		Checks  []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"checks"`
	}
	if err := json.Unmarshal([]byte(stdout.String()), &document); err != nil {
		t.Fatalf("doctor --json produced invalid JSON: %v\n%s", err, stdout.String())
	}
	if document.Kind != "doctor" {
		t.Errorf("kind = %q, want doctor", document.Kind)
	}
	if document.Changed {
		t.Error("doctor reported a change; inspection must never modify the host")
	}
	if len(document.Checks) == 0 {
		t.Fatal("doctor produced no checks")
	}
	for _, check := range document.Checks {
		// FIXED is provisioning's status. Doctor emitting it would mean it had
		// repaired something, which is exactly what it must not do.
		if check.Status == "FIXED" {
			t.Errorf("doctor reported %s as FIXED; it may only observe", check.ID)
		}
	}
}

func TestDoctorRejectsStrayArguments(t *testing.T) {
	var stdout, stderr strings.Builder
	if code := runDoctor([]string{"everything"}, &stdout, &stderr); code != 2 {
		t.Fatalf("stray argument exit = %d, want 2", code)
	}
	if code := runSetup([]string{"everything"}, &stdout, &stderr); code != 2 {
		t.Fatalf("stray argument exit = %d, want 2", code)
	}
}

func TestSetupDryRunNeitherRequiresRootNorChangesAnything(t *testing.T) {
	var stdout, stderr strings.Builder
	// A dry run is how an operator inspects what setup would do before letting
	// it near their host, so it must work without privilege.
	code := runSetup([]string{"--dry-run"}, &stdout, &stderr)
	if code != 0 && code != 1 {
		t.Fatalf("setup --dry-run exited %d (stderr %q)", code, stderr.String())
	}
	output := stdout.String()
	if !strings.Contains(output, "nothing was changed") {
		t.Errorf("a dry run must say plainly that it changed nothing:\n%s", output)
	}
	if strings.Contains(output, "Open http://") {
		t.Error("a dry run must not tell the operator the system is ready to use")
	}
}

func TestVersionAndHelpAreHandledWithoutStartingTheDaemon(t *testing.T) {
	for _, args := range [][]string{{"version"}, {"help"}, {"--help"}} {
		var stdout, stderr strings.Builder
		code, handled := dispatch(args, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("%v: handled=%v code=%d", args, handled, code)
		}
		if stdout.Len() == 0 {
			t.Errorf("%v produced no output", args)
		}
	}
}
