package main

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/llcoolkm/rookery/go/core"
	"os"
	"os/signal"
)

func main() {
	if len(os.Args) == 1 || len(os.Args) == 2 && (os.Args[1] == "--version" || os.Args[1] == "version") {
		fmt.Println("rookery " + core.Version + " (Go)")
		return
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	defer cancel()
	value, e := runPlatform(ctx, os.Args[1:])
	if e != nil {
		fmt.Fprintln(os.Stderr, "rookery: "+e.Error())
		os.Exit(1)
	}
	if value != nil {
		enc := json.NewEncoder(os.Stdout)
		enc.SetEscapeHTML(false)
		enc.SetIndent("", "  ")
		if enc.Encode(value) != nil {
			os.Exit(1)
		}
	}
}
