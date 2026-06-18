// Command vichu is the VichuFlow CLI: it initializes a project, runs observable
// agentic workflows, and inspects their persistent runtime.
package main

import (
	"fmt"
	"os"

	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

// Build metadata, injected via -ldflags by GoReleaser.
var (
	version = "dev"
	commit  = ""
	date    = ""
)

type command struct {
	name    string
	summary string // i18n key
	run     func(args []string) error
}

func commands() []command {
	return []command{
		{"init", "cmd.init", cmdInit},
		{"new", "cmd.new", cmdNew},
		{"uninstall", "cmd.uninstall", cmdUninstall},
		{"doctor", "cmd.doctor", cmdDoctor},
		{"run", "cmd.run", cmdRun},
		{"exec", "cmd.exec", cmdExec},
		{"worker", "cmd.worker", cmdWorker},
		{"review", "cmd.review", cmdReview},
		{"stage", "cmd.stage", cmdStage},
		{"status", "cmd.status", cmdStatus},
		{"observe", "cmd.observe", cmdObserve},
		{"resume", "cmd.resume", cmdResume},
		{"cancel", "cmd.cancel", cmdCancel},
		{"adapters", "cmd.adapters", cmdAdapters},
		{"config", "cmd.config", cmdConfig},
		{"version", "cmd.version", cmdVersion},
	}
}

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	name := os.Args[1]
	if name == "-h" || name == "--help" || name == "help" {
		usage()
		return
	}
	for _, c := range commands() {
		if c.name == name {
			if err := c.run(os.Args[2:]); err != nil {
				fmt.Fprintln(os.Stderr, i18n.T("cli.error")+err.Error())
				os.Exit(1)
			}
			return
		}
	}
	fmt.Fprintf(os.Stderr, i18n.T("cli.unknown_cmd")+"\n\n", name)
	usage()
	os.Exit(2)
}

func usage() {
	fmt.Println(i18n.T("cli.tagline"))
	fmt.Println()
	fmt.Println(i18n.T("cli.usage"))
	fmt.Println()
	fmt.Println(i18n.T("cli.commands"))
	for _, c := range commands() {
		fmt.Printf("  %-10s %s\n", c.name, i18n.T(c.summary))
	}
	fmt.Println()
	fmt.Println(i18n.T("cli.help_hint"))
}

func cmdVersion([]string) error {
	fmt.Printf(i18n.T("version.line")+"\n", version)
	if commit != "" {
		fmt.Printf(i18n.T("version.commit")+"\n", commit)
	}
	if date != "" {
		fmt.Printf(i18n.T("version.built")+"\n", date)
	}
	return nil
}
