package system

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestExitCodeDistinguishesWarningsFromFailure(t *testing.T) {
	var healthy Report
	healthy.OK("g", "a", "a", "")
	var warned Report
	warned.Warn("g", "b", "b", "", "")
	var failed Report
	failed.Fail("g", "c", "c", "", "")

	cases := []struct {
		name   string
		report Report
		strict bool
		want   int
	}{
		// A host that can collect but cannot back up is working, just not
		// fully provisioned. Calling that a failure by default would make the
		// exit code useless for the case it exists to catch.
		{"healthy", healthy, false, ExitHealthy},
		{"warnings are not failure by default", warned, false, ExitHealthy},
		{"warnings are failure under strict", warned, true, ExitProblem},
		{"errors always fail", failed, false, ExitProblem},
		{"errors fail under strict too", failed, true, ExitProblem},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ExitCode(tc.report, tc.strict); got != tc.want {
				t.Fatalf("ExitCode = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestJSONDocumentIsStableAndCarriesEveryPath(t *testing.T) {
	var report Report
	report.Fail(GroupHelper, "helper.installed", "Mount helper", "not installed", "Run setup.")
	document := NewDocument("doctor", report, Layout{User: "rotup", Group: "rotup"})

	var buffer strings.Builder
	if err := WriteJSON(&buffer, document); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(buffer.String()), &decoded); err != nil {
		t.Fatalf("doctor --json must emit valid JSON: %v", err)
	}
	for _, key := range []string{"kind", "ready", "changed", "worst", "checks", "layout"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("missing stable field %q in %s", key, buffer.String())
		}
	}
	if decoded["ready"] != false {
		t.Error("a report with an error must not be ready")
	}
	layout, _ := decoded["layout"].(map[string]any)
	for _, key := range []string{"helperPath", "sudoersPath", "tmpfilesPath", "unitPath", "runtimeDir", "secretRoot"} {
		if layout[key] == "" || layout[key] == nil {
			t.Errorf("layout must name %q so support can find it", key)
		}
	}
	checks, _ := decoded["checks"].([]any)
	first, _ := checks[0].(map[string]any)
	if first["id"] != "helper.installed" || first["remedy"] != "Run setup." {
		t.Errorf("a check must carry its stable id and its remedy: %v", first)
	}
}

func TestTextOutputGroupsChecksAndAnswersTheOperatorsQuestion(t *testing.T) {
	var report Report
	// Provisioning visits groups in dependency order, so the renderer has to
	// collect them rather than print a heading whenever the group changes.
	report.OK(GroupService, "service.unit", "systemd unit", "installed")
	report.Fixed(GroupHelper, "helper.installed", "Mount helper", "installed")
	report.OK(GroupService, "service.state", "Service state", "active")
	report.Warn(GroupTools, "tools.restic", "restic", "not found", "Install restic.")

	var buffer strings.Builder
	WriteText(&buffer, "ROTUP setup", report, Layout{User: "rotup"}, false)
	output := buffer.String()

	if strings.Count(output, GroupService+"\n") != 1 {
		t.Errorf("group heading repeated:\n%s", output)
	}
	if !strings.Contains(output, "System ready: YES, with warnings") {
		t.Errorf("output must answer whether the system is usable:\n%s", output)
	}
	if !strings.Contains(output, "Install restic.") {
		t.Errorf("a warning's remedy must be shown:\n%s", output)
	}

	report.Fail(GroupRuntime, "runtime.dir", "Runtime directory", "missing", "Run setup.")
	buffer.Reset()
	WriteText(&buffer, "ROTUP doctor", report, Layout{User: "rotup"}, false)
	if !strings.Contains(buffer.String(), "System ready: NO") {
		t.Errorf("an error must be reported as not ready:\n%s", buffer.String())
	}
}
