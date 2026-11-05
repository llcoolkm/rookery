//go:build windows

package windows

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/llcoolkm/rookery/go/platform"
)

func TestProcessIdentity(t *testing.T) {
	p, err := NewProcess()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	current, err := p.Current(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if current.PID != os.Getpid() || current.Principal == "" || current.Start == "" || current.BootID == "" {
		t.Fatal("incomplete pin")
	}
	another, err := NewProcess()
	if err != nil {
		t.Fatal(err)
	}
	same, err := another.Current(ctx)
	if err != nil || same != current {
		t.Fatal("boot or process identity is not stable")
	}
	if err = p.Pin(ctx, current); err != nil {
		t.Fatal(err)
	}
	for _, changed := range []platform.ProcessRef{
		{PID: current.PID, Principal: current.Principal, Start: current.Start + "0", BootID: current.BootID},
		{PID: current.PID, Principal: current.Principal, Start: current.Start, BootID: "different"},
		{PID: current.PID, Principal: "different", Start: current.Start, BootID: current.BootID},
	} {
		if p.Pin(ctx, changed) == nil {
			t.Fatal("changed identity accepted")
		}
	}
	image, err := p.ImagePath(ctx, current)
	if err != nil || !filepath.IsAbs(image) {
		t.Fatal("image unavailable", err)
	}
	ref, level, app, err := p.Security(ctx, current.PID)
	if err != nil || ref != current || level == 0 || app {
		t.Fatal("token security unavailable", err)
	}
	if _, err = p.Elevated(ctx, current.PID); err != nil {
		t.Fatal(err)
	}
	chain, err := p.Ancestors(ctx, current)
	if err != nil || len(chain) == 0 || chain[0] != current {
		t.Fatal("ancestor identity unavailable", err)
	}
	if _, err = p.CommandLine(ctx, current); err == nil {
		t.Fatal("unverified command line accepted")
	}
}

func TestWindowsNativeHelper(t *testing.T) {
	mode := os.Getenv("ROOKERY_WINDOWS_NATIVE_TEST")
	if mode == "" {
		return
	}
	switch mode {
	case "echo":
		io.Copy(os.Stdout, os.Stdin)
	case "limit":
		fmt.Fprint(os.Stdout, strings.Repeat("x", 32768))
		fmt.Fprint(os.Stderr, strings.Repeat("y", 32768))
	case "sleep":
		time.Sleep(30 * time.Second)
	case "pin":
		fmt.Fprintln(os.Stdout, "ready")
		io.Copy(io.Discard, os.Stdin)
	default:
		os.Exit(2)
	}
	os.Exit(0)
}

func TestProcessExitInvalidatesPin(t *testing.T) {
	p, err := NewProcess()
	if err != nil {
		t.Fatal(err)
	}
	image, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(image, "-test.run=^TestWindowsNativeHelper$")
	cmd.Env = append(os.Environ(), "ROOKERY_WINDOWS_NATIVE_TEST=pin")
	input, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	output, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { input.Close(); cmd.Process.Kill(); cmd.Wait() }()
	ready := make([]byte, 6)
	if _, err = io.ReadFull(output, ready); err != nil {
		t.Fatal(err)
	}
	ref, err := p.Lookup(context.Background(), cmd.Process.Pid)
	if err != nil {
		t.Fatal(err)
	}
	if err = p.Pin(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	input.Close()
	if err = cmd.Wait(); err != nil {
		t.Fatal(err)
	}
	if p.Pin(context.Background(), ref) == nil {
		t.Fatal("exited process accepted")
	}
}

func TestExecRefusals(t *testing.T) {
	executor := Executor{}
	for _, path := range []string{"relative.exe", `C:\synthetic\script.CmD`, `C:\synthetic\script.BAT`, `C:\Windows\System32\cmd.exe`, `C:\synthetic\powershell.exe`, `C:\synthetic\pwsh.exe`, `C:\synthetic\data.txt`, `\\server\share\program.exe`} {
		result, err := executor.Exec(context.Background(), platform.Spec{Path: path, Timeout: time.Second, MaxOutput: 100})
		if err == nil || result.State != platform.NotStarted {
			t.Fatal("unsafe executable accepted")
		}
	}
}

func TestExecBoundedNative(t *testing.T) {
	image, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	executor := Executor{}
	args := []string{"-test.run=^TestWindowsNativeHelper$"}
	t.Setenv("ROOKERY_WINDOWS_NATIVE_TEST", "echo")
	// Peer input travels through a pipe; it must not be added to Args.
	input := []byte("synthetic input with quotes \" and & | % !\n")
	result, err := executor.Exec(context.Background(), platform.Spec{Path: image, Args: args, Stdin: input, Timeout: 10 * time.Second, MaxOutput: 1024})
	if err != nil || result.State != platform.Completed || result.ExitCode != 0 || string(result.Stdout) != string(input) {
		t.Fatalf("native stdin failed: state=%d error=%v", result.State, err)
	}
	t.Setenv("ROOKERY_WINDOWS_NATIVE_TEST", "limit")
	result, err = executor.Exec(context.Background(), platform.Spec{Path: image, Args: args, Timeout: 10 * time.Second, MaxOutput: 128})
	if err == nil || result.State != platform.Uncertain || !result.Truncated || len(result.Stdout)+len(result.Stderr) > 128 {
		t.Fatal("output was not bounded")
	}
	t.Setenv("ROOKERY_WINDOWS_NATIVE_TEST", "sleep")
	started := time.Now()
	result, err = executor.Exec(context.Background(), platform.Spec{Path: image, Args: args, Timeout: 100 * time.Millisecond, MaxOutput: 128})
	if err == nil || result.State != platform.Uncertain || time.Since(started) > 10*time.Second {
		t.Fatal("timeout did not terminate job")
	}
}
