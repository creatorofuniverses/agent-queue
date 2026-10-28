package main

import (
	"os"

	"github.com/creatorofuniverses/agent-queue/internal/cli"
	"github.com/creatorofuniverses/agent-queue/internal/runner"
)

func main() {
	if len(os.Args) > 1 && (os.Args[1] == "_exec" || os.Args[1] == "_record-result") {
		os.Exit(runner.Main(os.Args[1], os.Args[2:]))
	}
	os.Exit(cli.Main(os.Args[1:], os.Getenv, os.Stdout, os.Stderr))
}
