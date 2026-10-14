// Command zemi is a self-improving, Lua-extensible AI agent harness for the
// terminal (DESIGN.md).
package main

import (
	"os"

	"github.com/metafates/zemi/internal/cli"
)

func main() {
	os.Exit(cli.Main(os.Args[1:], os.Stdout, os.Stderr))
}
