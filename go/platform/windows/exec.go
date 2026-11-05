//go:build windows

package windows

import (
	"bytes"
	"context"
	"debug/pe"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/llcoolkm/rookery/go/platform"
	win "golang.org/x/sys/windows"
)

// Executor starts explicit native images suspended, assigning a kill-on-close
// job before allowing user code to run. Only standard handles are inherited.
type Executor struct{}

func ResolveExecutable(name string) (string, error) {
	if strings.ContainsAny(name, "/\\\x00\r\n") {
		return "", errors.New("native executable unavailable")
	}
	path, err := exec.LookPath(strings.TrimSuffix(name, ".exe") + ".exe")
	if err != nil {
		return "", errors.New("native executable unavailable")
	}
	path, err = filepath.Abs(path)
	if err != nil {
		return "", errors.New("native executable unavailable")
	}
	handles, err := validateNativeImage(path)
	for _, h := range handles {
		win.CloseHandle(h)
	}
	if err != nil {
		return "", err
	}
	return path, nil
}

func validateNativeImage(path string) ([]win.Handle, error) {
	failure := errors.New("native executable refused")
	if !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") || strings.Contains(path[len(filepath.VolumeName(path)):], ":") || strings.HasPrefix(path, `\\`) || !strings.EqualFold(filepath.Ext(path), ".exe") {
		return nil, failure
	}
	switch strings.ToLower(filepath.Base(path)) {
	case "cmd.exe", "powershell.exe", "pwsh.exe", "wscript.exe", "cscript.exe", "mshta.exe", "bash.exe", "sh.exe", "zsh.exe", "dash.exe":
		return nil, failure
	}
	handles := []win.Handle{}
	fail := func() ([]win.Handle, error) {
		for _, h := range handles {
			win.CloseHandle(h)
		}
		return nil, failure
	}
	// Refuse reparse points and hold ancestry without FILE_SHARE_DELETE through
	// creation, preventing replacement of components already validated.
	for part := filepath.Clean(path); ; part = filepath.Dir(part) {
		wide, err := win.UTF16PtrFromString(part)
		if err != nil {
			return fail()
		}
		h, err := win.CreateFile(wide, win.FILE_READ_ATTRIBUTES, win.FILE_SHARE_READ|win.FILE_SHARE_WRITE, nil, win.OPEN_EXISTING, win.FILE_FLAG_OPEN_REPARSE_POINT|win.FILE_FLAG_BACKUP_SEMANTICS, 0)
		if err != nil {
			return fail()
		}
		handles = append(handles, h)
		var info win.ByHandleFileInformation
		if win.GetFileInformationByHandle(h, &info) != nil || info.FileAttributes&win.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return fail()
		}
		if filepath.Dir(part) == part {
			break
		}
	}
	wide, _ := win.UTF16PtrFromString(path)
	h, err := win.CreateFile(wide, win.GENERIC_READ, win.FILE_SHARE_READ, nil, win.OPEN_EXISTING, win.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if err != nil {
		return fail()
	}
	f := os.NewFile(uintptr(h), "")
	image, err := pe.NewFile(f)
	if err != nil {
		f.Close()
		return fail()
	}
	valid := image.FileHeader.Characteristics&pe.IMAGE_FILE_EXECUTABLE_IMAGE != 0 && image.FileHeader.Characteristics&pe.IMAGE_FILE_DLL == 0
	image.Close()
	if !valid {
		f.Close()
		return fail()
	}
	var held win.Handle
	err = win.DuplicateHandle(win.CurrentProcess(), h, win.CurrentProcess(), &held, 0, false, win.DUPLICATE_SAME_ACCESS)
	f.Close()
	if err != nil {
		return fail()
	}
	handles = append(handles, held)
	return handles, nil
}

// validateExecutable is also used by Scheduled Task installation.
func validateExecutable(path string) error {
	handles, err := validateNativeImage(path)
	for _, h := range handles {
		win.CloseHandle(h)
	}
	return err
}

type execCapture struct {
	mu             sync.Mutex
	left           int64
	stdout, stderr bytes.Buffer
	truncated      bool
	cancel         context.CancelFunc
}

func (c *execCapture) read(file *os.File, stderr bool, done chan<- struct{}) {
	defer func() { file.Close(); done <- struct{}{} }()
	buffer := make([]byte, 8192)
	for {
		n, err := file.Read(buffer)
		if n > 0 {
			c.mu.Lock()
			keep := int64(n)
			if keep > c.left {
				keep = c.left
				c.truncated = true
				c.cancel()
			}
			if stderr {
				c.stderr.Write(buffer[:keep])
			} else {
				c.stdout.Write(buffer[:keep])
			}
			c.left -= keep
			c.mu.Unlock()
		}
		if err != nil {
			return
		}
	}
}

func (Executor) Exec(ctx context.Context, spec platform.Spec) (platform.ExecResult, error) {
	failure := platform.ExecResult{State: platform.NotStarted}
	refused := errors.New("native execution refused")
	if spec.Timeout <= 0 || spec.MaxOutput <= 0 || ctx.Err() != nil {
		return failure, refused
	}
	for _, arg := range spec.Args {
		if strings.ContainsRune(arg, 0) {
			return failure, refused
		}
	}
	guarded, err := validateNativeImage(spec.Path)
	if err != nil {
		return failure, err
	}
	defer func() {
		for _, h := range guarded {
			win.CloseHandle(h)
		}
	}()
	app, err := win.UTF16PtrFromString(spec.Path)
	if err != nil {
		return failure, refused
	}
	command, err := win.UTF16FromString(win.ComposeCommandLine(append([]string{spec.Path}, spec.Args...)))
	if err != nil || len(command) > 32767 {
		return failure, refused
	}
	childCtx, cancel := context.WithTimeout(ctx, spec.Timeout)
	defer cancel()
	job, err := win.CreateJobObject(nil, nil)
	if err != nil {
		return failure, refused
	}
	defer win.CloseHandle(job)
	limits := win.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
	limits.BasicLimitInformation.LimitFlags = win.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err = win.SetInformationJobObject(job, win.JobObjectExtendedLimitInformation, uintptr(unsafe.Pointer(&limits)), uint32(unsafe.Sizeof(limits))); err != nil {
		return failure, refused
	}
	sa := win.SecurityAttributes{Length: uint32(unsafe.Sizeof(win.SecurityAttributes{})), InheritHandle: 1}
	var inRead, inWrite, outRead, outWrite, errRead, errWrite win.Handle
	opened := []win.Handle{}
	defer func() {
		for _, h := range opened {
			win.CloseHandle(h)
		}
	}()
	for _, pair := range [][2]*win.Handle{{&inRead, &inWrite}, {&outRead, &outWrite}, {&errRead, &errWrite}} {
		if win.CreatePipe(pair[0], pair[1], &sa, 0) != nil {
			return failure, refused
		}
		opened = append(opened, *pair[0], *pair[1])
	}
	for _, h := range []win.Handle{inWrite, outRead, errRead} {
		if win.SetHandleInformation(h, win.HANDLE_FLAG_INHERIT, 0) != nil {
			return failure, refused
		}
	}
	handles := []win.Handle{inRead, outWrite, errWrite}
	attrs, err := win.NewProcThreadAttributeList(1)
	if err != nil {
		return failure, refused
	}
	defer attrs.Delete()
	if attrs.Update(win.PROC_THREAD_ATTRIBUTE_HANDLE_LIST, unsafe.Pointer(&handles[0]), uintptr(len(handles))*unsafe.Sizeof(handles[0])) != nil {
		return failure, refused
	}
	startup := win.StartupInfoEx{StartupInfo: win.StartupInfo{Cb: uint32(unsafe.Sizeof(win.StartupInfoEx{})), Flags: win.STARTF_USESTDHANDLES, StdInput: inRead, StdOutput: outWrite, StdErr: errWrite}, ProcThreadAttributeList: attrs.List()}
	var proc win.ProcessInformation
	if win.CreateProcess(app, &command[0], nil, nil, true, win.CREATE_SUSPENDED|win.CREATE_NO_WINDOW|win.EXTENDED_STARTUPINFO_PRESENT, nil, nil, &startup.StartupInfo, &proc) != nil {
		return failure, errors.New("native executable did not start")
	}
	defer win.CloseHandle(proc.Process)
	defer win.CloseHandle(proc.Thread)
	if win.AssignProcessToJobObject(job, proc.Process) != nil {
		win.TerminateProcess(proc.Process, 1)
		return failure, refused
	}
	if _, err = win.ResumeThread(proc.Thread); err != nil {
		win.TerminateJobObject(job, 1)
		return platform.ExecResult{State: platform.Uncertain}, errors.New("native execution outcome uncertain")
	}
	win.CloseHandle(inRead)
	win.CloseHandle(outWrite)
	win.CloseHandle(errWrite)
	input := os.NewFile(uintptr(inWrite), "")
	output := os.NewFile(uintptr(outRead), "")
	errorOutput := os.NewFile(uintptr(errRead), "")
	opened = nil
	capture := &execCapture{left: spec.MaxOutput, cancel: cancel}
	readers := make(chan struct{}, 2)
	go capture.read(output, false, readers)
	go capture.read(errorOutput, true, readers)
	inputDone := make(chan struct{})
	go func() { io.Copy(input, bytes.NewReader(spec.Stdin)); input.Close(); close(inputDone) }()
	completed := false
	for childCtx.Err() == nil {
		state, waitErr := win.WaitForSingleObject(proc.Process, 20)
		if waitErr != nil {
			break
		}
		if state == win.WAIT_OBJECT_0 {
			completed = true
			break
		}
		if state != uint32(win.WAIT_TIMEOUT) {
			break
		}
	}
	// Descendants may retain standard handles after the original exits.
	win.TerminateJobObject(job, 1)
	input.Close()
	<-inputDone
	<-readers
	<-readers
	result := platform.ExecResult{State: platform.Uncertain}
	capture.mu.Lock()
	result.Stdout = append([]byte(nil), capture.stdout.Bytes()...)
	result.Stderr = append([]byte(nil), capture.stderr.Bytes()...)
	result.Truncated = capture.truncated
	capture.mu.Unlock()
	if !completed || childCtx.Err() != nil || result.Truncated {
		return result, errors.New("native execution outcome uncertain")
	}
	var code uint32
	if win.GetExitCodeProcess(proc.Process, &code) != nil {
		return result, errors.New("native execution outcome uncertain")
	}
	result.State = platform.Completed
	result.ExitCode = int(code)
	return result, nil
}

var _ platform.Executor = Executor{}
var _ platform.Process = (*Process)(nil)
