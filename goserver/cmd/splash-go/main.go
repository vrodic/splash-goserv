// Command splash-go is the Go port of the splash command
// (install/launcher.py): `splash-go serve` and the agent commands. It runs
// the Go server in its own process.
package main

import (
	"os"

	"splash/goserver/launcher"
)

func main() { os.Exit(launcher.Main(os.Args[1:])) }
