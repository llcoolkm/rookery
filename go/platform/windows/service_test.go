//go:build windows

package windows

import (
	"context"
	"encoding/xml"
	win "golang.org/x/sys/windows"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"unsafe"

	"github.com/llcoolkm/rookery/go/platform"
)

func TestScheduledTaskPlanOnly(t *testing.T) {
	host, err := New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	root, _ := host.Paths.StateDir()
	image, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	s := host.Service.(*service)
	spec := platform.ServiceSpec{Executable: image, StateDir: root}
	content, err := s.taskXML(spec)
	if err != nil {
		t.Fatal(err)
	}
	var task struct {
		XMLName    xml.Name
		Principals struct {
			Principal struct{ UserId, LogonType, RunLevel string }
		}
		Actions struct {
			Exec struct{ Command, Arguments string }
		}
	}
	if xml.Unmarshal([]byte(content), &task) != nil || task.XMLName.Local != "Task" {
		t.Fatal("invalid XML")
	}
	if task.Principals.Principal.UserId != s.paths.files.sid.String() || task.Principals.Principal.LogonType != "InteractiveToken" || task.Principals.Principal.RunLevel != "LeastPrivilege" {
		t.Fatal("wrong task principal")
	}
	if task.Actions.Exec.Command != image || !strings.Contains(task.Actions.Exec.Arguments, "daemon") {
		t.Fatal("wrong command")
	}
	if _, err = s.Plan(context.Background(), spec); err != nil {
		t.Fatal(err)
	}
	spec.Account = "other"
	if _, err = s.Plan(context.Background(), spec); err == nil {
		t.Fatal("cross-account task accepted")
	}
	spec.Account = ""
	spec.Executable = `C:\synthetic\script.cmd`
	if _, err = s.taskXML(spec); err == nil {
		t.Fatal("script accepted")
	}
}

// These mocks exercise the COM ABI without connecting to Task Scheduler or
// registering any task. The slots are from Microsoft's taskschd.h interfaces.
func TestScheduledTaskCOMVariantABI(t *testing.T) {
	if runtime.GOARCH != "amd64" {
		t.Skip("Task Scheduler COM backend supports amd64 only")
	}
	if unsafe.Sizeof(variant{}) != 24 || unsafe.Offsetof(variant{}.value) != 8 {
		t.Fatal("wrong VARIANT layout")
	}
	empty := variantArgument(variant{})
	text := variantArgument(variant{kind: 8, value: 123})
	for _, value := range []*variant{empty, text} {
		if uintptr(unsafe.Pointer(value))%16 != 0 {
			t.Fatal("unaligned VARIANT temporary")
		}
	}
	table := new([32]uintptr)
	object := &comObject{table: table}
	called := false
	table[16] = syscall.NewCallback(func(this *comObject, name, body, flags uintptr, user, password *variant, logon uintptr, security *variant, output **comObject) uintptr {
		called = true
		if this != object || name != 1 || body != 2 || flags != 18 || logon != 3 {
			t.Error("wrong RegisterTask signature")
		}
		if user.kind != 0 || password.kind != 0 {
			t.Error("unexpected credentials")
		}
		v := security
		if v.kind != 8 || v.value != 123 {
			t.Error("wrong security VARIANT")
		}
		return 0
	})
	var task *comObject
	if _, err := callCOM(object, 16, 1, 2, 18, uintptr(unsafe.Pointer(empty)), uintptr(unsafe.Pointer(empty)), 3, uintptr(unsafe.Pointer(text)), uintptr(unsafe.Pointer(&task))); err != nil || !called {
		t.Fatal("COM mock failed", err)
	}
	if _, err := callCOM(object, 31); err == nil {
		t.Fatal("missing COM method accepted")
	}
}

func TestScheduledTaskDescriptorRefusals(t *testing.T) {
	host, err := New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	s := host.Service.(*service)
	own := s.paths.files.sid.String()
	for _, test := range []struct {
		descriptor string
		ok         bool
	}{
		{"O:" + own + "D:P(A;;FA;;;" + own + ")(A;;FA;;;SY)", true},
		{"O:SYD:P(A;;FA;;;" + own + ")", false},
		{"O:" + own + "D:(A;;FA;;;" + own + ")", false},
		{"O:" + own + "D:P(A;;FA;;;WD)", false},
		{"O:" + own + "D:P", false},
	} {
		table := new([32]uintptr)
		table[21] = syscall.NewCallback(func(this *comObject, flags uintptr, output *uintptr) uintptr {
			if flags != win.OWNER_SECURITY_INFORMATION|win.DACL_SECURITY_INFORMATION {
				t.Error("wrong descriptor query")
			}
			// GetSecurityDescriptor transfers BSTR ownership to its caller.
			result, _, allocErr := bstr(test.descriptor)
			if allocErr != nil {
				return uintptr(0x80004005)
			}
			*output = result
			return 0
		})
		err = s.taskOwned(&comObject{table: table})
		if (err == nil) != test.ok {
			t.Fatal("descriptor ownership policy mismatch")
		}
	}
}
