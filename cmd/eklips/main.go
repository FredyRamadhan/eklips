// Command eklips is the daemon, CLI client, and optional TUI client binary.
package main

import (
	"context"
	"fmt"
	"os"

	"github.com/frdn/eklips/internal/apperr"
	"github.com/frdn/eklips/internal/cli"
)

func main() {
	if err := cli.Run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr); err != nil {
		ae := apperr.As(err)
		fmt.Fprintf(os.Stderr, "%s: %s\n", ae.Code, ae.Message)
		os.Exit(apperr.ExitCode(ae))
	}
}
