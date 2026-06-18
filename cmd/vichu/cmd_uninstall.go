package main

import (
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/corteshvictor/vichu-flow/internal/i18n"
)

// cmdUninstall removes an installed host pack, deleting only the files VichuFlow
// installed (by hash) and never the user's own edits.
func cmdUninstall(args []string) error {
	fs := flag.NewFlagSet("uninstall", flag.ContinueOnError)
	host := fs.String("host", "", i18n.T("init.flag_host"))
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *host == "" {
		return errors.New(i18n.T("uninstall.need_host"))
	}

	root, err := findRoot()
	if err != nil {
		cwd, gerr := os.Getwd()
		if gerr != nil {
			return gerr
		}
		root = cwd
	}

	removed, kept, err := uninstallHostPack(root, *host)
	if err != nil {
		return err
	}
	fmt.Printf(i18n.T("uninstall.done")+"\n", *host)
	for _, f := range removed {
		fmt.Printf("  - %s\n", f)
	}
	for _, f := range kept {
		fmt.Printf("  "+i18n.T("uninstall.kept")+"\n", f)
	}
	return nil
}
