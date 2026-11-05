//go:build linux

package linux

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/llcoolkm/rookery/go/platform"
)

type executor struct{}
type output struct {
	mu             sync.Mutex
	left           int64
	stdout, stderr bytes.Buffer
	truncated      bool
	cancel         context.CancelFunc
}
type outputWriter struct {
	out    *output
	stderr bool
}

func (w outputWriter) Write(data []byte) (int, error) {
	w.out.mu.Lock()
	defer w.out.mu.Unlock()
	n := len(data)
	keep := int64(n)
	if keep > w.out.left {
		keep = w.out.left
		w.out.truncated = true
		w.out.cancel()
	}
	if w.stderr {
		w.out.stderr.Write(data[:keep])
	} else {
		w.out.stdout.Write(data[:keep])
	}
	w.out.left -= keep
	return n, nil
}

// ResolveExecutable resolves a native binary once. The resulting absolute path
// is carried by Spec; execution never invokes a shell or shell fallback.
func ResolveExecutable(name string) (string, error) {
	path, e := exec.LookPath(name)
	if e != nil {
		return "", errors.New("native executable unavailable")
	}
	path, e = filepath.Abs(path)
	if e != nil {
		return "", errUnsafe
	}
	info, e := os.Stat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return "", errors.New("native executable unavailable")
	}
	return path, nil
}
func (e executor) Exec(ctx context.Context, spec platform.Spec) (platform.ExecResult, error) {
	failure := platform.ExecResult{State: platform.NotStarted}
	if !filepath.IsAbs(spec.Path) || spec.Timeout <= 0 || spec.MaxOutput <= 0 || strings.ContainsAny(spec.Path, "\r\n\x00") {
		return failure, errors.New("invalid native execution specification")
	}
	if ctx.Err() != nil {
		return failure, ctx.Err()
	}
	info, err := os.Stat(spec.Path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&0111 == 0 {
		return failure, errors.New("native executable unavailable")
	}
	// Executable scripts may invoke an implicit interpreter despite execve success.
	// Require a native ELF image; no shell resolution/fallback or shebang inputs.
	f, err := os.Open(spec.Path)
	if err != nil {
		return failure, errors.New("native executable unavailable")
	}
	var magic [4]byte
	n, _ := f.Read(magic[:])
	f.Close()
	if n != 4 || magic != [4]byte{0x7f, 'E', 'L', 'F'} {
		return failure, errors.New("native executable must be an ELF binary")
	}
	childCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	capture := &output{left: spec.MaxOutput, cancel: cancel}
	cmd := exec.CommandContext(childCtx, spec.Path, spec.Args...)
	cmd.Stdin = bytes.NewReader(spec.Stdin)
	cmd.Stdout = outputWriter{capture, false}
	cmd.Stderr = outputWriter{capture, true}
	cmd.WaitDelay = time.Second
	if err = cmd.Start(); err != nil {
		return failure, errors.New("native executable did not start")
	}
	err = cmd.Wait()
	result := platform.ExecResult{State: platform.Uncertain, Stdout: capture.stdout.Bytes(), Stderr: capture.stderr.Bytes(), Truncated: capture.truncated}
	if childCtx.Err() != nil || capture.truncated || errors.Is(err, exec.ErrWaitDelay) {
		return result, errors.New("native execution outcome uncertain")
	}
	if err == nil {
		result.State = platform.Completed
		result.ExitCode = 0
		return result, nil
	}
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		result.State = platform.Completed
		result.ExitCode = exit.ExitCode()
		return result, nil
	}
	return result, errors.New("native execution outcome uncertain")
}
