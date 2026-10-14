package main

import (
	"os"

	"github.com/slaghuis/pipeline-lib/cli"
)

func main() { os.Exit(cli.Run(os.Args[1:])) }
