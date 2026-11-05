//go:build windows

package windows

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	win "golang.org/x/sys/windows"
	"io"
	"os"
	"path/filepath"
	"testing"
	"unsafe"
)

func testFiles(t *testing.T) (*files, string) {
	t.Helper()
	f, e := newFiles()
	if e != nil {
		t.Fatal(e)
	}
	root := filepath.Join(t.TempDir(), "state")
	if _, e = f.PrivateState(context.Background(), root); e != nil {
		t.Fatal(e)
	}
	return f, root
}
func TestPrivateDACLAndAtomicReplacement(t *testing.T) {
	f, root := testFiles(t)
	ctx := context.Background()
	for _, data := range [][]byte{[]byte("first"), bytes.Repeat([]byte("complete record\n"), 8192)} {
		if e := f.AtomicWrite(ctx, root, "store.json", data); e != nil {
			t.Fatal(e)
		}
		v, e := f.OpenNoFollow(ctx, root, "store.json", false)
		if e != nil {
			t.Fatal(e)
		}
		got, e := io.ReadAll(v)
		if e != nil {
			t.Fatal(e)
		}
		if !bytes.Equal(got, data) {
			t.Fatal("partial replacement")
		}
		sd, e := win.GetSecurityInfo(win.Handle(v.(*verifiedFile).Fd()), win.SE_FILE_OBJECT, win.OWNER_SECURITY_INFORMATION|win.DACL_SECURITY_INFORMATION)
		if e != nil {
			t.Fatal(e)
		}
		control, _, e := sd.Control()
		if e != nil || control&win.SE_DACL_PROTECTED == 0 {
			t.Fatal("inherited DACL")
		}
		acl, _, e := sd.DACL()
		if e != nil || acl == nil || acl.AceCount != 1 {
			t.Fatal("unexpected ACEs")
		}
		var ace *win.ACCESS_ALLOWED_ACE
		if win.GetAce(acl, 0, &ace) != nil || !(*win.SID)(unsafe.Pointer(&ace.SidStart)).Equals(f.sid) {
			t.Fatal("foreign grant")
		}
		v.Close()
	}
	if e := f.AtomicWriteExclusive(ctx, root, "store.json", []byte("changed")); !errors.Is(e, os.ErrExist) {
		t.Fatal("exclusive overwrite admitted", e)
	}
}
func TestRefuseReparseAndTraversal(t *testing.T) {
	f, root := testFiles(t)
	ctx := context.Background()
	for _, name := range []string{`..\outside`, `C:\outside`, `file:stream`, `file.`, `NUL`} {
		if _, e := f.OpenNoFollow(ctx, root, name, true); e == nil {
			t.Fatal("unsafe name admitted")
		}
	}
	target := filepath.Join(root, "target")
	if _, e := f.PrivateState(ctx, target); e != nil {
		t.Fatal(e)
	}
	link := filepath.Join(root, "link")
	createJunction(t, f, link, target)
	if _, e := f.PrivateState(ctx, link); e == nil {
		t.Fatal("reparse state admitted")
	}
	if _, e := f.OpenNoFollow(ctx, root, `link\record`, true); e == nil {
		t.Fatal("reparse ancestry admitted")
	}
}
func TestLockFileExExclusion(t *testing.T) {
	f, root := testFiles(t)
	ctx := context.Background()
	first, e := f.AdvisoryLock(ctx, root, "admission.lock", false)
	if e != nil {
		t.Fatal(e)
	}
	if second, e := f.AdvisoryLock(ctx, root, "admission.lock", false); e == nil {
		second.Close()
		t.Fatal("concurrent lock admitted")
	}
	first.Close()
	second, e := f.AdvisoryLock(ctx, root, "admission.lock", false)
	if e != nil {
		t.Fatal(e)
	}
	second.Close()
}
func TestRecordCapabilityLifetime(t *testing.T) {
	f, root := testFiles(t)
	ctx := context.Background()
	if e := f.AtomicWrite(ctx, root, "record.json", []byte("{}")); e != nil {
		t.Fatal(e)
	}
	v, e := f.OpenProviderRecord(ctx, root, "record.json")
	if e != nil {
		t.Fatal(e)
	}
	real := v.(*verifiedFile)
	if recordProvenance(real) != nil {
		t.Fatal("valid provenance refused")
	}
	v.Close()
	if recordProvenance(real) == nil {
		t.Fatal("closed record admitted")
	}
}

func TestProviderRecordRotationAndAncestryRename(t *testing.T) {
	f, root := testFiles(t)
	ctx := context.Background()
	if e := f.AtomicWrite(ctx, root, "record.json", []byte("old")); e != nil {
		t.Fatal(e)
	}
	v, e := f.OpenProviderRecord(ctx, root, "record.json")
	if e != nil {
		t.Fatal(e)
	}
	defer v.Close()
	if v.(*verifiedFile).File != nil || len(v.(*verifiedFile).guards) != 0 {
		t.Fatal("provider snapshot retained native handles")
	}
	if e = os.Rename(root, root+"-moved"); e != nil {
		t.Fatal("snapshot retained handles blocking ancestry rename", e)
	}
	if recordProvenance(v.(*verifiedFile)) == nil {
		t.Fatal("record at missing original ancestry admitted")
	}
	createJunction(t, f, root, root+"-moved")
	if recordProvenance(v.(*verifiedFile)) == nil {
		t.Fatal("same record through substituted ancestry admitted")
	}
	if e = os.Remove(root); e != nil {
		t.Fatal(e)
	}
	if e = os.Rename(root+"-moved", root); e != nil {
		t.Fatal(e)
	}
	if recordProvenance(v.(*verifiedFile)) != nil {
		t.Fatal("restored original identity refused")
	}
	if e = f.AtomicWrite(ctx, root, "record.json", []byte("new")); e != nil {
		t.Fatal("snapshot retained handles blocking atomic rotation", e)
	}
	if recordProvenance(v.(*verifiedFile)) == nil {
		t.Fatal("replaced record identity admitted")
	}
	got, e := io.ReadAll(v)
	if e != nil || string(got) != "old" {
		t.Fatal("snapshot did not retain original record", e)
	}
	fresh, e := f.OpenProviderRecord(ctx, root, "record.json")
	if e != nil {
		t.Fatal(e)
	}
	defer fresh.Close()
	if recordProvenance(fresh.(*verifiedFile)) != nil {
		t.Fatal("new verified snapshot refused")
	}
	got, e = io.ReadAll(fresh)
	if e != nil || string(got) != "new" {
		t.Fatal("next read did not capture rotated record", e)
	}
	if e = f.AtomicWrite(ctx, root, "record.json", []byte("new")); e != nil {
		t.Fatal(e)
	}
	if recordProvenance(fresh.(*verifiedFile)) == nil {
		t.Fatal("different file identity with identical bytes admitted")
	}
}

func TestProviderSnapshotRevalidationAndLimits(t *testing.T) {
	f, root := testFiles(t)
	ctx := context.Background()
	if e := f.AtomicWrite(ctx, root, "record.json", []byte("old")); e != nil {
		t.Fatal(e)
	}
	v, e := f.OpenProviderRecord(ctx, root, "record.json")
	if e != nil {
		t.Fatal(e)
	}
	defer v.Close()
	if _, e := v.Write([]byte("changed")); e == nil {
		t.Fatal("snapshot write admitted")
	}
	if e := v.Sync(); e == nil {
		t.Fatal("snapshot sync claimed durable storage")
	}
	if e := os.WriteFile(filepath.Join(root, "record.json"), []byte("new"), 0600); e != nil {
		t.Fatal("snapshot blocked in-place edit", e)
	}
	if recordProvenance(v.(*verifiedFile)) == nil {
		t.Fatal("in-place changed record admitted")
	}
	if e := f.AtomicWrite(ctx, root, "large.json", bytes.Repeat([]byte("x"), 1<<20)); e != nil {
		t.Fatal(e)
	}
	large, e := f.OpenProviderRecord(ctx, root, "large.json")
	if e != nil {
		t.Fatal("maximum-size record refused", e)
	}
	large.Close()
	if e := os.WriteFile(filepath.Join(root, "large.json"), bytes.Repeat([]byte("x"), (1<<20)+1), 0600); e != nil {
		t.Fatal(e)
	}
	if large, e = f.OpenProviderRecord(ctx, root, "large.json"); e == nil {
		large.Close()
		t.Fatal("oversized record admitted")
	}
	if e := os.Rename(root, root+"-moved"); e != nil {
		t.Fatal("snapshot failure retained handles", e)
	}
}
func TestRemoveVerifiedHandle(t *testing.T) {
	f, root := testFiles(t)
	ctx := context.Background()
	if e := f.AtomicWrite(ctx, root, "record", []byte("x")); e != nil {
		t.Fatal(e)
	}
	if e := f.Remove(ctx, root, "record"); e != nil {
		t.Fatal(e)
	}
	if _, e := f.OpenNoFollow(ctx, root, "record", false); !errors.Is(e, os.ErrNotExist) {
		t.Fatal("record remains", e)
	}
}

func TestRejectForeignACLGrant(t *testing.T) {
	f, root := testFiles(t)
	ctx := context.Background()
	if e := f.AtomicWrite(ctx, root, "record", []byte("x")); e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(root, "record")
	descriptor, e := win.SecurityDescriptorFromString("D:P(A;;FA;;;" + f.sid.String() + ")(A;;FR;;;WD)")
	if e != nil {
		t.Fatal(e)
	}
	acl, _, e := descriptor.DACL()
	if e != nil {
		t.Fatal(e)
	}
	if e = win.SetNamedSecurityInfo(path, win.SE_FILE_OBJECT, win.DACL_SECURITY_INFORMATION|win.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, acl, nil); e != nil {
		t.Fatal(e)
	}
	if v, e := f.OpenNoFollow(ctx, root, "record", false); e == nil {
		v.Close()
		t.Fatal("foreign read grant admitted for private state")
	}
}
func TestPortableRelativeRecordNames(t *testing.T) {
	for _, name := range []string{"sessions/record.json", `sessions\record.json`, "settings.json"} {
		if !validRelative(name) {
			t.Fatal("portable record name refused")
		}
	}
	for _, name := range []string{"../record", "sessions/../record", "sessions//record", `sessions\..\record`} {
		if validRelative(name) {
			t.Fatal("traversal admitted")
		}
	}
}

// NTFS directory junctions require no symbolic-link privilege or Developer Mode.
// Only disposable test directories are modified. The mount-point reparse buffer
// is the documented REPARSE_DATA_BUFFER layout (offsets relative to PathBuffer).
func createJunction(t *testing.T, f *files, link, target string) {
	t.Helper()
	if _, e := f.PrivateState(context.Background(), link); e != nil {
		t.Fatal(e)
	}
	path, _ := win.UTF16PtrFromString(link)
	h, e := win.CreateFile(path, win.GENERIC_WRITE, win.FILE_SHARE_READ|win.FILE_SHARE_WRITE, nil, win.OPEN_EXISTING, win.FILE_FLAG_OPEN_REPARSE_POINT|win.FILE_FLAG_BACKUP_SEMANTICS, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer win.CloseHandle(h)
	substitute, e := win.UTF16FromString(`\??\` + target)
	if e != nil {
		t.Fatal(e)
	}
	printName, e := win.UTF16FromString(target)
	if e != nil {
		t.Fatal(e)
	}
	payload := append(substitute, printName...)
	buffer := make([]byte, 16+2*len(payload))
	binary.LittleEndian.PutUint32(buffer, win.IO_REPARSE_TAG_MOUNT_POINT)
	binary.LittleEndian.PutUint16(buffer[4:], uint16(8+2*len(payload)))
	binary.LittleEndian.PutUint16(buffer[8:], 0)
	binary.LittleEndian.PutUint16(buffer[10:], uint16(2*(len(substitute)-1)))
	binary.LittleEndian.PutUint16(buffer[12:], uint16(2*len(substitute)))
	binary.LittleEndian.PutUint16(buffer[14:], uint16(2*(len(printName)-1)))
	for i, v := range payload {
		binary.LittleEndian.PutUint16(buffer[16+2*i:], v)
	}
	var returned uint32
	if e = win.DeviceIoControl(h, win.FSCTL_SET_REPARSE_POINT, &buffer[0], uint32(len(buffer)), nil, 0, &returned, nil); e != nil {
		t.Fatal(e)
	}
}
