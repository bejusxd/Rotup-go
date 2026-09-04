package system

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
)

// Exit codes. These are the command's contract with a monitoring script, so
// they are deliberately few and documented in docs/operations/installation.md.
const (
	// ExitHealthy means no ERROR was found. Warnings alone still exit 0 unless
	// strict is requested, because a host that can collect but not back up is
	// working, just not fully provisioned.
	ExitHealthy = 0
	// ExitProblem means at least one ERROR was found, or a warning was found
	// while strict was requested.
	ExitProblem = 1
)

// ExitCode maps a report to a process exit status.
func ExitCode(report Report, strict bool) int {
	if !report.Ready() {
		return ExitProblem
	}
	if strict && report.Worst() == StatusWarning {
		return ExitProblem
	}
	return ExitHealthy
}

// Document is the machine-readable form of a run. It is a stable shape: fields
// are added, never renamed or removed, so a support script written today keeps
// working.
type Document struct {
	// Kind is "doctor" or "setup".
	Kind string `json:"kind"`
	// Ready is false when any check is an ERROR.
	Ready bool `json:"ready"`
	// Changed is true when provisioning altered the host. Doctor is always
	// false, which is its whole contract.
	Changed bool `json:"changed"`
	// Worst is the most demanding status found.
	Worst Status `json:"worst"`
	// Checks are every check in the order they were performed.
	Checks []Check `json:"checks"`
	// Layout records the account and paths the run was made against.
	Layout DocumentLayout `json:"layout"`
}

// DocumentLayout is the resolved installation identity, for support.
type DocumentLayout struct {
	User           string `json:"user"`
	Group          string `json:"group"`
	HelperPath     string `json:"helperPath"`
	WrapperPath    string `json:"wrapperPath"`
	SudoersPath    string `json:"sudoersPath"`
	TmpfilesPath   string `json:"tmpfilesPath"`
	UnitPath       string `json:"unitPath"`
	EnvFilePath    string `json:"envFilePath"`
	DataDir        string `json:"dataDir"`
	StagingRoot    string `json:"stagingRoot"`
	SecretRoot     string `json:"secretRoot"`
	RuntimeDir     string `json:"runtimeDir"`
	MediaMountRoot string `json:"mediaMountRoot"`
}

// NewDocument builds the machine-readable form.
func NewDocument(kind string, report Report, layout Layout) Document {
	return Document{
		Kind:    kind,
		Ready:   report.Ready(),
		Changed: report.Changed(),
		Worst:   report.Worst(),
		Checks:  report.Checks,
		Layout: DocumentLayout{
			User: layout.User, Group: layout.Group,
			HelperPath: HelperPath, WrapperPath: WrapperPath, SudoersPath: SudoersPath,
			TmpfilesPath: TmpfilesPath, UnitPath: UnitPath, EnvFilePath: EnvFilePath,
			DataDir: DataDir, StagingRoot: StagingRoot, SecretRoot: SecretRoot,
			RuntimeDir: RuntimeDir, MediaMountRoot: MediaMountRoot,
		},
	}
}

// WriteJSON emits the machine-readable form.
func WriteJSON(w io.Writer, document Document) error {
	encoder := json.NewEncoder(w)
	encoder.SetIndent("", "  ")
	return encoder.Encode(document)
}

// marker is the leading glyph for a status. ASCII alternatives are not offered:
// the daemon already logs UTF-8, and a status is also always spelled out in the
// text beside it, so nothing is lost if a terminal renders the glyph poorly.
func marker(status Status) string {
	switch status {
	case StatusOK:
		return "  ok "
	case StatusFixed:
		return " fix "
	case StatusWarning:
		return "warn "
	default:
		return "FAIL "
	}
}

// WriteText renders a run for a person. It groups checks, states what was
// observed, and ends with the single question an operator came to answer.
func WriteText(w io.Writer, title string, report Report, layout Layout, strict bool) {
	fmt.Fprintf(w, "%s\n\n", title)

	// Collect by group before printing. Provisioning visits the helper, the
	// runtime directory and the service in an order driven by dependencies, so
	// printing in that order would repeat a heading whenever two interleave.
	var order []string
	byGroup := map[string][]Check{}
	for _, check := range report.Checks {
		if _, seen := byGroup[check.Group]; !seen {
			order = append(order, check.Group)
		}
		byGroup[check.Group] = append(byGroup[check.Group], check)
	}
	for _, group := range order {
		fmt.Fprintf(w, "%s\n", group)
		for _, check := range byGroup[group] {
			fmt.Fprintf(w, "  %s%-28s %s\n", marker(check.Status), check.Title, check.Detail)
		}
	}

	failures := report.Failures()
	if len(failures) > 0 {
		fmt.Fprintf(w, "\nWhat to do next\n")
		for _, check := range failures {
			if check.Remedy == "" {
				continue
			}
			fmt.Fprintf(w, "  %s %s\n", strings.TrimSpace(string(check.Status))+":", check.Title)
			for _, line := range wrap(check.Remedy, 72) {
				fmt.Fprintf(w, "      %s\n", line)
			}
		}
	}

	fmt.Fprintf(w, "\n")
	switch {
	case !report.Ready():
		fmt.Fprintf(w, "System ready: NO\n")
	case strict && report.Worst() == StatusWarning:
		fmt.Fprintf(w, "System ready: NO (warnings, and --strict was requested)\n")
	case report.Worst() == StatusWarning:
		fmt.Fprintf(w, "System ready: YES, with warnings\n")
	default:
		fmt.Fprintf(w, "System ready: YES\n")
	}
}

// wrap breaks a remedy into terminal-width lines without splitting words.
func wrap(text string, width int) []string {
	words := strings.Fields(text)
	if len(words) == 0 {
		return nil
	}
	lines := []string{words[0]}
	for _, word := range words[1:] {
		last := len(lines) - 1
		if len(lines[last])+1+len(word) <= width {
			lines[last] += " " + word
			continue
		}
		lines = append(lines, word)
	}
	return lines
}
