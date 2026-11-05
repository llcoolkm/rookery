//go:build windows

package windows

import (
	"context"
	"errors"
	"os"
	"strconv"
	"unsafe"

	"github.com/llcoolkm/rookery/go/platform"
	win "golang.org/x/sys/windows"
)

var errProcess = errors.New("process identity unavailable")

// Process pins processes using an open handle, token SID and creation FILETIME.
// Toolhelp is used only to obtain parent PIDs, never as identity evidence.
type Process struct{ principal, boot string }

func NewProcess() (*Process, error) {
	token, err := win.OpenCurrentProcessToken()
	if err != nil {
		return nil, errProcess
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil {
		return nil, errProcess
	}
	boot, err := bootIdentity()
	if err != nil {
		return nil, err
	}
	return &Process{principal: user.User.Sid.String(), boot: boot}, nil
}

// bootIdentity uses the kernel's boot GUID, not uptime or rounded wall time.
// This native information class is queried only for boot identity: process
// pinning never uses NtQueryInformationProcess or a process snapshot. An absent
// API, changed layout, failed query or zero GUID fails closed.
func bootIdentity() (string, error) {
	query := win.NewLazySystemDLL("ntdll.dll").NewProc("NtQuerySystemInformation")
	if query.Find() != nil {
		return "", errProcess
	}
	var value struct {
		ID       win.GUID
		Firmware uint32
		Padding  uint32
		Flags    uint64
	}
	var size uint32
	status, _, _ := query.Call(uintptr(win.SystemBootEnvironmentInformation), uintptr(unsafe.Pointer(&value)), unsafe.Sizeof(value), uintptr(unsafe.Pointer(&size)))
	if status != 0 || size != uint32(unsafe.Sizeof(value)) || value.ID == (win.GUID{}) {
		return "", errProcess
	}
	return value.ID.String(), nil
}

func (p *Process) Current(ctx context.Context) (platform.ProcessRef, error) {
	return p.Lookup(ctx, os.Getpid())
}

func (p *Process) inspect(ctx context.Context, pid int, security bool) (platform.ProcessRef, uint32, bool, error) {
	if err := ctx.Err(); err != nil {
		return platform.ProcessRef{}, 0, false, err
	}
	if pid <= 0 || uint64(pid) > uint64(^uint32(0)) || p == nil || p.principal == "" || p.boot == "" {
		return platform.ProcessRef{}, 0, false, errProcess
	}
	handle, err := win.OpenProcess(win.PROCESS_QUERY_LIMITED_INFORMATION|win.SYNCHRONIZE, false, uint32(pid))
	if err != nil {
		return platform.ProcessRef{}, 0, false, errProcess
	}
	defer win.CloseHandle(handle)
	var created, exited, kernel, userTime win.Filetime
	if win.GetProcessTimes(handle, &created, &exited, &kernel, &userTime) != nil || exited != (win.Filetime{}) {
		return platform.ProcessRef{}, 0, false, errProcess
	}
	var token win.Token
	if win.OpenProcessToken(handle, win.TOKEN_QUERY, &token) != nil {
		return platform.ProcessRef{}, 0, false, errProcess
	}
	defer token.Close()
	user, err := token.GetTokenUser()
	if err != nil || user.User.Sid.String() != p.principal {
		return platform.ProcessRef{}, 0, false, errProcess
	}
	ref := platform.ProcessRef{PID: pid, Principal: p.principal, Start: strconv.FormatUint(uint64(created.HighDateTime)<<32|uint64(created.LowDateTime), 10), BootID: p.boot}
	var integrity uint32
	var container bool
	if security {
		var length uint32
		if err = win.GetTokenInformation(token, win.TokenIntegrityLevel, nil, 0, &length); err != win.ERROR_INSUFFICIENT_BUFFER || length < uint32(unsafe.Sizeof(win.Tokenmandatorylabel{})) || length > 65536 {
			return platform.ProcessRef{}, 0, false, errProcess
		}
		buffer := make([]byte, length)
		if win.GetTokenInformation(token, win.TokenIntegrityLevel, &buffer[0], length, &length) != nil {
			return platform.ProcessRef{}, 0, false, errProcess
		}
		label := (*win.Tokenmandatorylabel)(unsafe.Pointer(&buffer[0]))
		if label.Label.Sid == nil || !label.Label.Sid.IsValid() || label.Label.Sid.SubAuthorityCount() == 0 {
			return platform.ProcessRef{}, 0, false, errProcess
		}
		integrity = label.Label.Sid.SubAuthority(uint32(label.Label.Sid.SubAuthorityCount() - 1))
		var flag uint32
		if win.GetTokenInformation(token, 29 /* TokenIsAppContainer */, (*byte)(unsafe.Pointer(&flag)), 4, &length) != nil || length != 4 {
			return platform.ProcessRef{}, 0, false, errProcess
		}
		container = flag != 0
	}
	state, err := win.WaitForSingleObject(handle, 0)
	if err != nil || state != uint32(win.WAIT_TIMEOUT) {
		return platform.ProcessRef{}, 0, false, errProcess
	}
	return ref, integrity, container, nil
}

func (p *Process) Lookup(ctx context.Context, pid int) (platform.ProcessRef, error) {
	ref, _, _, err := p.inspect(ctx, pid, false)
	return ref, err
}

// Security captures SID, integrity RID, AppContainer status and process identity
// from the same open process handle. SID equality is required, not assumed.
func (p *Process) Security(ctx context.Context, pid int) (platform.ProcessRef, uint32, bool, error) {
	return p.inspect(ctx, pid, true)
}
func (p *Process) Pin(ctx context.Context, ref platform.ProcessRef) error {
	live, err := p.Lookup(ctx, ref.PID)
	if err != nil {
		return err
	}
	if live != ref {
		return errProcess
	}
	return nil
}

func (p *Process) ImagePath(ctx context.Context, ref platform.ProcessRef) (string, error) {
	if p.Pin(ctx, ref) != nil {
		return "", errProcess
	}
	handle, err := win.OpenProcess(win.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(ref.PID))
	if err != nil {
		return "", errProcess
	}
	defer win.CloseHandle(handle)
	var created, exited, kernel, user win.Filetime
	if win.GetProcessTimes(handle, &created, &exited, &kernel, &user) != nil || strconv.FormatUint(uint64(created.HighDateTime)<<32|uint64(created.LowDateTime), 10) != ref.Start {
		return "", errProcess
	}
	buffer := make([]uint16, 32768)
	length := uint32(len(buffer))
	if win.QueryFullProcessImageName(handle, 0, &buffer[0], &length) != nil || length == 0 || p.Pin(ctx, ref) != nil {
		return "", errProcess
	}
	return win.UTF16ToString(buffer[:length]), nil
}

func (p *Process) Ancestors(ctx context.Context, ref platform.ProcessRef) ([]platform.ProcessRef, error) {
	result := []platform.ProcessRef{}
	seen := map[int]bool{}
	for len(result) < 1024 {
		if seen[ref.PID] || p.Pin(ctx, ref) != nil {
			return nil, errProcess
		}
		seen[ref.PID] = true
		result = append(result, ref)
		snapshot, err := win.CreateToolhelp32Snapshot(win.TH32CS_SNAPPROCESS, 0)
		if err != nil {
			return nil, errProcess
		}
		entry := win.ProcessEntry32{Size: uint32(unsafe.Sizeof(win.ProcessEntry32{}))}
		var parent uint32
		found := false
		for err = win.Process32First(snapshot, &entry); err == nil; err = win.Process32Next(snapshot, &entry) {
			if entry.ProcessID == uint32(ref.PID) {
				parent = entry.ParentProcessID
				found = true
				break
			}
		}
		win.CloseHandle(snapshot)
		if !found || p.Pin(ctx, ref) != nil {
			return nil, errProcess
		}
		if parent == 0 {
			return result, nil
		}
		next, err := p.Lookup(ctx, int(parent))
		if err != nil {
			return result, nil
		}
		// A reused parent PID has a later creation time than its purported child.
		parentStart, pe := strconv.ParseUint(next.Start, 10, 64)
		childStart, ce := strconv.ParseUint(ref.Start, 10, 64)
		if pe != nil || ce != nil || parentStart > childStart {
			return nil, errProcess
		}
		ref = next
	}
	return nil, errProcess
}

func (p *Process) CommandLine(ctx context.Context, ref platform.ProcessRef) ([]string, error) {
	if p.Pin(ctx, ref) != nil {
		return nil, errProcess
	}
	// There is no documented handle-based arbitrary-process argv API. Do not read
	// PEB memory or use NtQueryInformationProcess as an identity shortcut.
	return nil, errors.New("Windows process command line inspection unsupported")
}

// Elevated returns the token elevation bit used by Codex's Windows peer policy.
func (p *Process) Elevated(ctx context.Context, pid int) (bool, error) {
	ref, err := p.Lookup(ctx, pid)
	if err != nil {
		return false, err
	}
	handle, err := win.OpenProcess(win.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false, errProcess
	}
	defer win.CloseHandle(handle)
	var token win.Token
	if win.OpenProcessToken(handle, win.TOKEN_QUERY, &token) != nil {
		return false, errProcess
	}
	defer token.Close()
	var elevation, length uint32
	if win.GetTokenInformation(token, win.TokenElevation, (*byte)(unsafe.Pointer(&elevation)), 4, &length) != nil || length != 4 || p.Pin(ctx, ref) != nil {
		return false, errProcess
	}
	return elevation != 0, nil
}
