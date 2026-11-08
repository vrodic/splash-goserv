// Command splash-server is the Go port of Splash's Python HTTP server
// (python -m server.server): it takes the same command line and drives the
// same native engine.
package main

import (
	"os"

	"splash/goserver/server"
)

func main() { os.Exit(server.Main(os.Args[1:])) }
