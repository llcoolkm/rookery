//go:build windows

package main

import (
	"context"
	"errors"
)

func runPlatform(context.Context, []string) (any, error) {
	return nil, errors.New("Windows helper runtime is deferred to phase 3; use the Python helper on Linux")
}
