//go:build windows

package windows

// Task Scheduler's native COM interfaces avoid shells, XML files and credentials
// in argv. VARIANT arguments use the Windows x64 aggregate-by-reference ABI.
// Vtable order and signatures are verified against Microsoft's taskschd.h:
// https://github.com/microsoft/win32metadata/blob/main/generation/WinSDK/RecompiledIdlHeaders/um/taskschd.h
// ABI: https://learn.microsoft.com/en-us/cpp/build/x64-calling-convention
// Other architectures fail closed until their COM aggregate ABI is implemented.
import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"github.com/llcoolkm/rookery/go/platform"
	win "golang.org/x/sys/windows"
)

const TaskName = "rookery-go"

var errTask = errors.New("Scheduled Task operation unavailable")
var ole32 = win.NewLazySystemDLL("ole32.dll")
var oleaut32 = win.NewLazySystemDLL("oleaut32.dll")

type comObject struct{ table *[32]uintptr }
type variant struct {
	kind     uint16
	reserved [3]uint16
	value    uint64
	// The VARIANT union includes a two-pointer BRECORD on 64-bit Windows.
	recordInfo uint64
}

// variantArgument creates the 16-byte aligned temporary required for passing
// a 24-byte VARIANT aggregate by reference under the Windows x64 ABI.
func variantArgument(value variant) *variant {
	storage := new([40]byte)
	base := unsafe.Pointer(storage)
	aligned := unsafe.Add(base, (16-uintptr(base)%16)%16)
	result := (*variant)(aligned)
	*result = value
	return result
}

//go:uintptrescapes
func callCOM(o *comObject, index int, args ...uintptr) (uintptr, error) {
	if o == nil || o.table == nil || index < 0 || index >= len(o.table) || o.table[index] == 0 {
		return 0, errTask
	}
	all := append([]uintptr{uintptr(unsafe.Pointer(o))}, args...)
	result, _, _ := syscall.SyscallN(o.table[index], all...)
	runtime.KeepAlive(o)
	if int32(result) < 0 {
		return result, errTask
	}
	return result, nil
}
func releaseCOM(o *comObject) {
	if o != nil {
		_, _ = callCOM(o, 2)
	}
}
func bstr(s string) (uintptr, func(), error) {
	chars, err := win.UTF16FromString(s)
	if err != nil {
		return 0, nil, errTask
	}
	p, _, _ := oleaut32.NewProc("SysAllocStringLen").Call(uintptr(unsafe.Pointer(&chars[0])), uintptr(len(chars)-1))
	if p == 0 {
		return 0, nil, errTask
	}
	return p, func() { oleaut32.NewProc("SysFreeString").Call(p) }, nil
}

// scheduler keeps all COM work on one initialized apartment/thread.
func scheduler(ctx context.Context, fn func(*comObject) error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if runtime.GOARCH != "amd64" {
		return errTask
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	hr, _, _ := ole32.NewProc("CoInitializeEx").Call(0, 0)
	if int32(hr) < 0 {
		return errTask
	}
	defer ole32.NewProc("CoUninitialize").Call()
	clsid := win.GUID{Data1: 0x0f87369f, Data2: 0xa4e5, Data3: 0x4cfc, Data4: [8]byte{0xbd, 0x3e, 0x73, 0xe6, 0x15, 0x45, 0x72, 0xdd}}
	iid := win.GUID{Data1: 0x2faba4c7, Data2: 0x4da9, Data3: 0x4013, Data4: [8]byte{0x96, 0x97, 0x20, 0xcc, 0x3f, 0xd4, 0x0f, 0x85}}
	var svc *comObject
	hr, _, _ = ole32.NewProc("CoCreateInstance").Call(uintptr(unsafe.Pointer(&clsid)), 0, 1, uintptr(unsafe.Pointer(&iid)), uintptr(unsafe.Pointer(&svc)))
	if int32(hr) < 0 {
		return errTask
	}
	defer releaseCOM(svc)
	empty := variantArgument(variant{})
	if _, err := callCOM(svc, 10, uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(empty))); err != nil {
		return err
	}
	root, free, err := bstr(`\`)
	if err != nil {
		return err
	}
	defer free()
	var folder *comObject
	if _, err = callCOM(svc, 7, root, uintptr(unsafe.Pointer(&folder))); err != nil {
		return err
	}
	defer releaseCOM(folder)
	return fn(folder)
}

// taskOwned refuses to control an existing task unless its security descriptor
// proves this account owns its protected ACL and only this account/SYSTEM have
// allowed access. A name alone never grants authority over a preexisting task.
func (s *service) taskOwned(task *comObject) error {
	var raw *uint16
	if _, err := callCOM(task, 21, win.OWNER_SECURITY_INFORMATION|win.DACL_SECURITY_INFORMATION, uintptr(unsafe.Pointer(&raw))); err != nil || raw == nil {
		return errTask
	}
	defer oleaut32.NewProc("SysFreeString").Call(uintptr(unsafe.Pointer(raw)))
	text := win.UTF16PtrToString(raw)
	sd, err := win.SecurityDescriptorFromString(text)
	if err != nil {
		return errTask
	}
	owner, _, err := sd.Owner()
	if err != nil || !owner.Equals(s.paths.files.sid) {
		return errTask
	}
	control, _, err := sd.Control()
	if err != nil || control&win.SE_DACL_PROTECTED == 0 {
		return errTask
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount == 0 {
		return errTask
	}
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *win.ACCESS_ALLOWED_ACE
		if win.GetAce(acl, i, &ace) != nil {
			return errTask
		}
		if ace.Header.AceType != win.ACCESS_ALLOWED_ACE_TYPE {
			return errTask
		}
		sid := (*win.SID)(unsafe.Pointer(&ace.SidStart))
		if !sid.IsValid() || (!sid.Equals(s.paths.files.sid) && sid.String() != "S-1-5-18") {
			return errTask
		}
	}
	return nil
}

type service struct{ paths *paths }

func xmlText(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
func (s *service) ConfigureBackend(backend string) error {
	if backend != "auto" && backend != "scheduled-task" {
		return errTask
	}
	return nil
}
func (s *service) taskXML(spec platform.ServiceSpec) (string, error) {
	if spec.Account != "" || spec.Home != "" || spec.SearchPath != "" || spec.StateDir != s.paths.state {
		return "", errTask
	}
	if err := validateExecutable(spec.Executable); err != nil {
		return "", err
	}
	args := append([]string{"--state-dir", spec.StateDir}, spec.Arguments...)
	if len(spec.Arguments) == 0 {
		args = append(args, "daemon")
	}
	for i, arg := range args {
		if strings.ContainsAny(arg, "\x00\r\n") {
			return "", errTask
		}
		args[i] = syscall.EscapeArg(arg)
	}
	sid := s.paths.files.sid.String()
	return fmt.Sprintf(`<?xml version="1.0"?><Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task"><Triggers><LogonTrigger><Enabled>true</Enabled><UserId>%s</UserId></LogonTrigger></Triggers><Principals><Principal id="Owner"><UserId>%s</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals><Settings><MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy><DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries><StopIfGoingOnBatteries>false</StopIfGoingOnBatteries><ExecutionTimeLimit>PT0S</ExecutionTimeLimit><RestartOnFailure><Interval>PT1M</Interval><Count>3</Count></RestartOnFailure></Settings><Actions Context="Owner"><Exec><Command>%s</Command><Arguments>%s</Arguments></Exec></Actions></Task>`, sid, sid, xmlText(spec.Executable), xmlText(strings.Join(args, " "))), nil
}
func (s *service) Plan(ctx context.Context, spec platform.ServiceSpec) (any, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	content, err := s.taskXML(spec)
	if err != nil {
		return nil, err
	}
	return map[string]any{"backend": "scheduled-task", "name": TaskName, "content": content, "started": false}, nil
}
func (s *service) Install(ctx context.Context, spec platform.ServiceSpec) error {
	text, err := s.taskXML(spec)
	if err != nil {
		return err
	}
	return scheduler(ctx, func(folder *comObject) error {
		name, fn, err := bstr(TaskName)
		if err != nil {
			return err
		}
		defer fn()
		body, fb, err := bstr(text)
		if err != nil {
			return err
		}
		defer fb()
		// Explicit current-owner and SYSTEM DACL. TASK_CREATE refuses overwrite.
		acl, fa, err := bstr("O:" + s.paths.files.sid.String() + "D:P(A;;FA;;;" + s.paths.files.sid.String() + ")(A;;FA;;;SY)")
		if err != nil {
			return err
		}
		defer fa()
		empty := variantArgument(variant{})
		security := variantArgument(variant{kind: 8, value: uint64(acl)})
		var task *comObject
		_, err = callCOM(folder, 16, name, body, 18, uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(empty)), 3, uintptr(unsafe.Pointer(security)), uintptr(unsafe.Pointer(&task)))
		defer releaseCOM(task)
		if err != nil {
			return err
		}
		return s.verifyCreatedTask(folder, name, task)
	})
}

// A successful TASK_CREATE grants cleanup authority over this new task even
// when Task Scheduler did not preserve the requested descriptor. Never use
// this rollback path for a preexisting task.
func (s *service) verifyCreatedTask(folder *comObject, name uintptr, task *comObject) error {
	if err := s.taskOwned(task); err != nil {
		if _, cleanupErr := callCOM(folder, 15, name, 0); cleanupErr != nil {
			return errors.Join(err, fmt.Errorf("new Scheduled Task cleanup failed: %w", cleanupErr))
		}
		return err
	}
	return nil
}

func (s *service) Uninstall(ctx context.Context) error {
	return scheduler(ctx, func(folder *comObject) error {
		name, free, err := bstr(TaskName)
		if err != nil {
			return err
		}
		defer free()
		var task *comObject
		if _, err = callCOM(folder, 13, name, uintptr(unsafe.Pointer(&task))); err != nil {
			return err
		}
		defer releaseCOM(task)
		if err = s.taskOwned(task); err != nil {
			return err
		}
		_, err = callCOM(folder, 15, name, 0)
		return err
	})
}
func (s *service) Status(ctx context.Context) (platform.ServiceStatus, error) {
	result := platform.ServiceStatus{Detail: "scheduled-task"}
	err := scheduler(ctx, func(folder *comObject) error {
		name, free, err := bstr(TaskName)
		if err != nil {
			return err
		}
		defer free()
		var task *comObject
		hr, err := callCOM(folder, 13, name, uintptr(unsafe.Pointer(&task)))
		if uint32(hr) == 0x80070002 {
			return nil
		}
		if err != nil {
			return err
		}
		defer releaseCOM(task)
		if err = s.taskOwned(task); err != nil {
			return err
		}
		var state uint32
		if _, err = callCOM(task, 9, uintptr(unsafe.Pointer(&state))); err != nil {
			return err
		}
		result.Installed = true
		result.Running = state == 4
		return nil
	})
	return result, err
}
func (s *service) Start(ctx context.Context) error { return s.action(ctx, true) }
func (s *service) Stop(ctx context.Context) error  { return s.action(ctx, false) }
func (s *service) action(ctx context.Context, start bool) error {
	return scheduler(ctx, func(folder *comObject) error {
		name, free, err := bstr(TaskName)
		if err != nil {
			return err
		}
		defer free()
		var task *comObject
		if _, err = callCOM(folder, 13, name, uintptr(unsafe.Pointer(&task))); err != nil {
			return err
		}
		defer releaseCOM(task)
		if err = s.taskOwned(task); err != nil {
			return err
		}
		if !start {
			_, err = callCOM(task, 23, 0)
			return err
		}
		empty := variantArgument(variant{})
		var running *comObject
		_, err = callCOM(task, 12, uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(&running)))
		releaseCOM(running)
		return err
	})
}

var _ platform.Service = (*service)(nil)
