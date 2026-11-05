// Command rookery is the Go helper entry point. CLI parity arrives in phase 2.
package main

import (
	"fmt"
	"github.com/llcoolkm/rookery/go/core"
	"os"
)

func main() {
	if len(os.Args) == 1 || (len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version")) {
		fmt.Println("rookery " + core.Version + " (Go phase 1)")
		return
	}
	fmt.Fprintln(os.Stderr, "rookery: helper commands are not implemented; TODO phase 2 CLI parity")
	os.Exit(2)
}
