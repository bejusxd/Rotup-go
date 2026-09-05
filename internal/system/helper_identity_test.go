package system

import (
	"context"
	"reflect"
	"testing"
)

type helperIdentityHost struct {
	Host
	euid          int
	commands      [][]string
	serviceDenied bool
}

func (h *helperIdentityHost) Euid() int { return h.euid }

func (h *helperIdentityHost) Run(_ context.Context, name string, args ...string) (string, int, error) {
	h.commands = append(h.commands, append([]string{name}, args...))
	// Direct root invocation succeeds even when the service account cannot
	// elevate. This models the false positive this regression prevents.
	if name == "/usr/bin/sudo" && h.serviceDenied {
		return "sudo: a password is required", 1, nil
	}
	return `{"error_code":"usage"}`, 1, nil
}

func TestHelperProbeUsesSelectedAccount(t *testing.T) {
	for _, tc := range []struct {
		name    string
		euid    int
		denied  bool
		want    Status
		command []string
	}{
		{"root with authorized service account", 0, false, StatusOK, []string{"/usr/bin/sudo", "-n", "-u", "rotup", "--", WrapperPath}},
		{"root success must not hide service account denial", 0, true, StatusError, []string{"/usr/bin/sudo", "-n", "-u", "rotup", "--", WrapperPath}},
		{"selected account directly", 1000, false, StatusOK, []string{WrapperPath}},
		{"different unprivileged account", 2000, false, StatusError, nil},
		{"unknown effective identity", -1, false, StatusError, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host := &helperIdentityHost{Host: newTestHost(t), euid: tc.euid, serviceDenied: tc.denied}
			var report Report
			probeHelperInvocation(context.Background(), host, "rotup", &report)
			if got := find(t, report, "helper.invocation").Status; got != tc.want {
				t.Fatalf("status = %s, want %s", got, tc.want)
			}
			var want [][]string
			if tc.command != nil {
				want = [][]string{tc.command}
			}
			if !reflect.DeepEqual(host.commands, want) {
				t.Fatalf("commands = %v, want %v", host.commands, want)
			}
		})
	}
}

func TestHelperProbeDoesNotRunForUnresolvedAccount(t *testing.T) {
	host := &helperIdentityHost{Host: newTestHost(t), euid: 0}
	var report Report
	probeHelperInvocation(context.Background(), host, "missing", &report)
	if find(t, report, "helper.invocation").Status != StatusError || len(host.commands) != 0 {
		t.Fatal("unresolved identity must fail without running a probe")
	}
}
