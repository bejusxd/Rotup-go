package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/bejusxd/rotup/internal/config"
	"github.com/bejusxd/rotup/internal/system"
)

// Subcommands. With no subcommand rotupd runs the daemon, which is what the
// systemd unit does and what every existing deployment expects.
const (
	commandSetup   = "setup"
	commandDoctor  = "doctor"
	commandVersion = "version"
	commandHelp    = "help"
)

const usage = `rotupd — ROTUP local backup daemon

Usage:
  rotupd                  run the daemon (the systemd unit's ExecStart)
  rotupd setup [flags]    provision this host's Linux integration; needs root
  rotupd doctor [flags]   inspect this host and change nothing
  rotupd version          print the build version

setup flags:
  --user <name>       account the daemon runs as (default: the account that
                      invoked sudo, else the current account). It must exist;
                      setup never creates an account.
  --helper <path>     built rotup-mounthelper to install root-owned
  --daemon <path>     built rotupd to install at /usr/local/bin/rotupd
  --service           install the systemd unit without enabling it
  --enable-service    install, enable and start the unit (implies --service)
  --force             replace an environment file an operator has edited
  --dry-run           report every change and make none
  --json              machine-readable output
  --strict            exit non-zero on warnings as well as errors

doctor flags:
  --user <name>       account to check against (default as above)
  --active            also run version probes and one no-argument helper
                      invocation. Still never mounts, writes application
                      state, or touches a registered disk.
  --json              machine-readable output
  --strict            exit non-zero on warnings as well as errors

Exit status is 0 when no error was found and 1 otherwise; with --strict a
warning is also non-zero.`

// dispatch routes a subcommand. It returns the process exit code and whether it
// handled the arguments at all; false means "run the daemon".
func dispatch(args []string, stdout, stderr io.Writer) (int, bool) {
	if len(args) == 0 {
		return 0, false
	}
	switch args[0] {
	case commandSetup:
		return runSetup(args[1:], stdout, stderr), true
	case commandDoctor:
		return runDoctor(args[1:], stdout, stderr), true
	case commandVersion:
		fmt.Fprintf(stdout, "rotupd %s\n", version)
		return 0, true
	case commandHelp, "-h", "--help":
		fmt.Fprintln(stdout, usage)
		return 0, true
	}
	fmt.Fprintf(stderr, "unknown command %q\n\n%s\n", args[0], usage)
	return 2, true
}

// toolOverrides reads the same configuration variables the daemon uses, so
// doctor and setup check the paths this host will actually run with rather than
// whatever happens to be on PATH.
func toolOverrides(host system.Host) system.Options {
	options := system.Options{}
	if value, ok := host.LookupEnv(config.EnvRclonePath); ok {
		options.RclonePath = value
	}
	if value, ok := host.LookupEnv(config.EnvResticPath); ok {
		options.ResticPath = value
	}
	if value, ok := host.LookupEnv(config.EnvLsblkPath); ok {
		options.LsblkPath = value
	}
	if value, ok := host.LookupEnv(config.EnvStagingRoot); ok {
		options.StagingRoot = value
	}
	if value, ok := host.LookupEnv(config.EnvSecretRoot); ok {
		options.SecretRoot = value
	}
	if value, ok := host.LookupEnv(config.EnvDBPath); ok {
		options.DatabasePath = value
	}
	return options
}

func runDoctor(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet(commandDoctor, flag.ContinueOnError)
	set.SetOutput(stderr)
	user := set.String("user", "", "account the daemon runs as")
	active := set.Bool("active", false, "run version probes and one helper invocation")
	asJSON := set.Bool("json", false, "machine-readable output")
	strict := set.Bool("strict", false, "treat warnings as failure")
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintf(stderr, "doctor takes no positional arguments\n")
		return 2
	}

	host := system.NewOSHost()
	options := toolOverrides(host)
	options.User = *user
	options.Active = *active

	inspection := system.Inspect(context.Background(), host, options)
	if *asJSON {
		if err := system.WriteJSON(stdout, system.NewDocument(commandDoctor, inspection.Report, inspection.Layout)); err != nil {
			fmt.Fprintf(stderr, "output could not be encoded: %v\n", err)
			return 2
		}
		return system.ExitCode(inspection.Report, *strict)
	}
	system.WriteText(stdout, "ROTUP doctor", inspection.Report, inspection.Layout, *strict)
	if !*active {
		fmt.Fprintf(stdout, "\nRun `rotupd doctor --active` to probe tool versions and the helper invocation.\n")
	}
	return system.ExitCode(inspection.Report, *strict)
}

func runSetup(args []string, stdout, stderr io.Writer) int {
	set := flag.NewFlagSet(commandSetup, flag.ContinueOnError)
	set.SetOutput(stderr)
	user := set.String("user", "", "account the daemon runs as")
	helper := set.String("helper", "", "built rotup-mounthelper to install")
	daemon := set.String("daemon", "", "built rotupd to install")
	service := set.Bool("service", false, "install the systemd unit without enabling it")
	enable := set.Bool("enable-service", false, "install, enable and start the systemd unit")
	force := set.Bool("force", false, "replace an edited environment file")
	dryRun := set.Bool("dry-run", false, "report changes without making them")
	asJSON := set.Bool("json", false, "machine-readable output")
	strict := set.Bool("strict", false, "treat warnings as failure")
	if err := set.Parse(args); err != nil {
		return 2
	}
	if set.NArg() != 0 {
		fmt.Fprintf(stderr, "setup takes no positional arguments\n")
		return 2
	}

	host := system.NewOSHost()
	options := system.SetupOptions{
		Options:        toolOverrides(host),
		HelperSource:   *helper,
		DaemonSource:   *daemon,
		InstallService: *service || *enable,
		EnableService:  *enable,
		Force:          *force,
		DryRun:         *dryRun,
	}
	options.User = *user
	// Provisioning always probes what it installed: a setup that reported
	// success without running the helper once would be reporting its own
	// intentions rather than the result.
	options.Active = !*dryRun

	report, inspection := system.Setup(context.Background(), host, options)

	if *asJSON {
		// The provisioning actions and the verification that followed them are
		// one document, so a support bundle shows both what changed and what
		// the host looked like afterwards.
		combined := system.Report{Checks: append(append([]system.Check{}, report.Checks...), inspection.Report.Checks...)}
		if err := system.WriteJSON(stdout, system.NewDocument(commandSetup, combined, inspection.Layout)); err != nil {
			fmt.Fprintf(stderr, "output could not be encoded: %v\n", err)
			return 2
		}
		return system.ExitCode(combined, *strict)
	}

	title := "ROTUP setup"
	if *dryRun {
		title = "ROTUP setup (dry run — nothing was changed)"
	}
	system.WriteText(stdout, title, report, inspection.Layout, *strict)

	fmt.Fprintf(stdout, "\nVerification after provisioning\n\n")
	system.WriteText(stdout, "ROTUP doctor", inspection.Report, inspection.Layout, *strict)

	if inspection.Report.Ready() && !*dryRun {
		address := "127.0.0.1:8080"
		if value, ok := host.LookupEnv(config.EnvHTTPAddr); ok && value != "" {
			address = value
		}
		fmt.Fprintf(stdout, "\nOpen http://%s/setup to configure a source and run the first backup.\n", address)
	}

	combined := system.Report{Checks: append(append([]system.Check{}, report.Checks...), inspection.Report.Checks...)}
	return system.ExitCode(combined, *strict)
}

// exitWithUsage is used by main when the daemon itself refuses to start.
func fprintErr(format string, args ...any) {
	_, _ = fmt.Fprintf(os.Stderr, format, args...)
}
