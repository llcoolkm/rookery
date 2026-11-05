//go:build linux

package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/llcoolkm/rookery/go/platform"
)

func agyEnv(h platform.Host) agyEnvironment {
	return agyEnvironment{getenv: os.Getenv, self: os.Executable,
		ancestor: func(ctx context.Context) (platform.ProcessRef, error) {
			p, ok := h.Process.(interface {
				AGYAncestor(context.Context) (platform.ProcessRef, error)
			})
			if !ok {
				return platform.ProcessRef{}, errors.New("AGY process inspection unsupported")
			}
			return p.AGYAncestor(ctx)
		}, classify: func(ctx context.Context, id, artifact string) (string, error) {
			p, ok := h.Paths.(interface {
				AGYConversation(context.Context, string, string) (string, error)
			})
			if !ok {
				return "unknown", nil
			}
			return p.AGYConversation(ctx, id, artifact)
		}, spawn: spawnAGY}
}
func agyCommand(ctx context.Context, h platform.Host, root string, a arguments) (any, error) {
	env := agyEnv(h)
	if a.command == "agy-hook" {
		return agyHookWith(ctx, h, root, a.pos(0), os.Stdin, env)
	}
	if a.options["foreground"] == "true" && a.options["agy-ready"] == "true" {
		pipe := os.NewFile(3, "agy-ready")
		if pipe == nil {
			return nil, errors.New("AGY readiness channel unavailable")
		}
		defer pipe.Close()
		env.ready = func(v any) error { return json.NewEncoder(pipe).Encode(v) }
	}
	return agyCommandWith(ctx, h, root, a, env)
}
func agyPlatformStatus(ctx context.Context, h platform.Host, root string) any {
	return agyStatus(ctx, h, root)
}
func spawnAGY(ctx context.Context, id, name, root string) (any, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, errors.New("helper executable unavailable")
	}
	read, write, err := os.Pipe()
	if err != nil {
		return nil, errors.New("AGY readiness channel unavailable")
	}
	defer read.Close()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		write.Close()
		return nil, errors.New("AGY endpoint startup failed")
	}
	defer null.Close()
	// Public options only. Inherited AGY credentials are never inspected or copied
	// into arguments. The child also has no native sender.
	cmd := exec.Command(self, "--state-dir", root, "attach", "agy", "--foreground", "--agy-ready", "--conversation", id, "--name", name)
	cmd.Stdin = null
	cmd.Stdout = null
	cmd.Stderr = null
	cmd.ExtraFiles = []*os.File{write}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err = cmd.Start(); err != nil {
		write.Close()
		return nil, errors.New("AGY endpoint startup failed")
	}
	write.Close()
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	ready := make(chan struct {
		value any
		err   error
	}, 1)
	go func() {
		var v map[string]any
		err := json.NewDecoder(io.LimitReader(read, 4097)).Decode(&v)
		ready <- struct {
			value any
			err   error
		}{v, err}
	}()
	timer := time.NewTimer(10 * time.Second)
	defer timer.Stop()
	select {
	case r := <-ready:
		if r.err == nil {
			if v, ok := r.value.(map[string]any); ok && v["attached"] == "agy:"+id && v["delivery"] == "held" {
				return v, nil
			}
		}
	case <-done:
	case <-ctx.Done():
	case <-timer.C:
	}
	_ = cmd.Process.Kill()
	return nil, errors.New("AGY endpoint did not report readiness; inspect attach agy --status")
}
