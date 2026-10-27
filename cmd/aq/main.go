package main

import (
	"os"

	"resource-dispatcher/internal/cli"
	"resource-dispatcher/internal/runner"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "_exec" || os.Args[1] == "_record-result") {
		os.Exit(runner.Main(os.Args[1], os.Args[2:]))
	}
	os.Exit(cli.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
