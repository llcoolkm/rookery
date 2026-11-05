//go:build windows

package windows

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unsafe"

	"github.com/llcoolkm/rookery/go/platform"
	win "golang.org/x/sys/windows"
)

var errUnsafe = errors.New("unsafe local object")

type files struct {
	sid        *win.SID
	descriptor *win.SECURITY_DESCRIPTOR
	mu         sync.Mutex
	opened     map[*verifiedFile]bool
}

type verifiedFile struct {
	*os.File
	host           *files
	root, relative string
	guards         []win.Handle
}

func (v *verifiedFile) Close() error {
	v.host.mu.Lock()
	delete(v.host.opened, v)
	v.host.mu.Unlock()
	err := v.File.Close()
	closeGuards(v.guards)
	v.guards = nil
	return err
}
func newFiles() (*files, error) {
	user, err := win.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, errUnsafe
	}
	sid, err := user.User.Sid.Copy()
	if err != nil {
		return nil, errUnsafe
	}
	// SYSTEM is optional in the contract; only the current SID is granted here.
	sd, err := win.SecurityDescriptorFromString("O:" + sid.String() + "D:P(A;;FA;;;" + sid.String() + ")")
	if err != nil {
		return nil, errUnsafe
	}
	return &files{sid: sid, descriptor: sd, opened: map[*verifiedFile]bool{}}, nil
}
func (f *files) attributes() *win.SecurityAttributes {
	return &win.SecurityAttributes{Length: uint32(unsafe.Sizeof(win.SecurityAttributes{})), SecurityDescriptor: f.descriptor}
}
func validPath(path string) bool {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || strings.HasPrefix(path, `\\`) {
		return false
	}
	vol := filepath.VolumeName(path)
	if len(vol) != 2 || vol[1] != ':' {
		return false
	}
	for _, p := range strings.Split(strings.TrimPrefix(path, vol+`\`), `\`) {
		if p != "" && !validComponent(p) {
			return false
		}
	}
	return true
}
func validComponent(p string) bool {
	if p == "" || p == "." || p == ".." || strings.ContainsAny(p, "/\\:\x00<>\"|?*") || strings.TrimRight(p, " .") != p {
		return false
	}
	for _, r := range p {
		if r < 32 {
			return false
		}
	}
	base := strings.ToUpper(strings.SplitN(p, ".", 2)[0])
	switch base {
	case "CON", "PRN", "AUX", "NUL", "CONIN$", "CONOUT$", "COM¹", "COM²", "COM³", "LPT¹", "LPT²", "LPT³":
		return false
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) && base[3] >= '0' && base[3] <= '9' {
		return false
	}
	return true
}
func validRelative(name string) bool {
	// Portable callers use slash-separated relative record names. Normalize only
	// the separator; cleaning must not erase traversal or ambiguous components.
	native := filepath.FromSlash(name)
	if native == "" || filepath.IsAbs(native) || filepath.Clean(native) != native {
		return false
	}
	for _, part := range strings.Split(native, `\`) {
		if !validComponent(part) {
			return false
		}
	}
	return true
}

func closeGuards(handles []win.Handle) {
	for i := len(handles) - 1; i >= 0; i-- {
		win.CloseHandle(handles[i])
	}
}

func (f *files) validateHandle(h win.Handle, private, directory bool) error {
	var st win.ByHandleFileInformation
	if win.GetFileInformationByHandle(h, &st) != nil || st.FileAttributes&win.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
		return errUnsafe
	}
	if (st.FileAttributes&win.FILE_ATTRIBUTE_DIRECTORY != 0) != directory {
		return errUnsafe
	}
	if !directory && st.NumberOfLinks != 1 {
		return errUnsafe
	}
	sd, err := win.GetSecurityInfo(h, win.SE_FILE_OBJECT, win.OWNER_SECURITY_INFORMATION|win.DACL_SECURITY_INFORMATION)
	if err != nil {
		return errUnsafe
	}
	owner, _, err := sd.Owner()
	if err != nil {
		return errUnsafe
	}
	own := owner.Equals(f.sid)
	// Windows system ancestry is trusted only when owned by these well-known
	// administrative principals; provider files and private objects require SID.
	trusted := func(s *win.SID) bool {
		v := s.String()
		return v == "S-1-5-18" || v == "S-1-5-32-544" || v == "S-1-5-80-956008885-3418522649-1831038044-1853292631-2271478464"
	}
	if !own && (!directory || !trusted(owner)) {
		return errUnsafe
	}
	control, _, err := sd.Control()
	if err != nil || (private && control&win.SE_DACL_PROTECTED == 0) {
		return errUnsafe
	}
	acl, _, err := sd.DACL()
	if err != nil || acl == nil || acl.AceCount == 0 {
		return errUnsafe
	}
	const mutations = win.FILE_WRITE_DATA | win.FILE_APPEND_DATA | win.FILE_WRITE_EA | win.FILE_WRITE_ATTRIBUTES | 0x40 /* FILE_DELETE_CHILD */ | win.DELETE | win.WRITE_DAC | win.WRITE_OWNER | win.GENERIC_WRITE | win.GENERIC_ALL
	granted := false
	for i := uint32(0); i < uint32(acl.AceCount); i++ {
		var ace *win.ACCESS_ALLOWED_ACE
		if win.GetAce(acl, i, &ace) != nil {
			return errUnsafe
		}
		if ace.Header.AceType == win.ACCESS_DENIED_ACE_TYPE {
			continue
		}
		if ace.Header.AceType != win.ACCESS_ALLOWED_ACE_TYPE {
			return errUnsafe
		}
		if ace.Header.AceFlags&0x08 != 0 {
			continue
		} // INHERIT_ONLY_ACE
		sid := (*win.SID)(unsafe.Pointer(&ace.SidStart))
		if sid.Equals(f.sid) {
			granted = true
			continue
		}
		if private {
			return errUnsafe
		}
		if uint32(ace.Mask)&mutations != 0 && !trusted(sid) {
			return errUnsafe
		}
	}
	if private && !granted {
		return errUnsafe
	}
	return nil
}

// guardPath holds every component open without FILE_SHARE_DELETE. Replacement,
// rename and reparse substitution of any traversed component therefore fail
// until the operation ends. Private files retain guards for their lifetime;
// provider records release them after open and recheck identity at provenance.
func (f *files) guardPath(path string, private, create bool) ([]win.Handle, error) {
	if !validPath(path) {
		return nil, errUnsafe
	}
	volume := filepath.VolumeName(path) + `\`
	parts := strings.Split(strings.TrimPrefix(path, volume), `\`)
	paths := []string{volume}
	for _, part := range parts {
		if part != "" {
			paths = append(paths, filepath.Join(paths[len(paths)-1], part))
		}
	}
	var guards []win.Handle
	for i, p := range paths {
		ptr, _ := win.UTF16PtrFromString(p)
		h, err := win.CreateFile(ptr, win.FILE_READ_ATTRIBUTES|win.READ_CONTROL, win.FILE_SHARE_READ|win.FILE_SHARE_WRITE, nil, win.OPEN_EXISTING, win.FILE_FLAG_BACKUP_SEMANTICS|win.FILE_FLAG_OPEN_REPARSE_POINT, 0)
		if errors.Is(err, win.ERROR_FILE_NOT_FOUND) || errors.Is(err, win.ERROR_PATH_NOT_FOUND) {
			if create && i > 0 {
				err = win.CreateDirectory(ptr, f.attributes())
				if err == nil || errors.Is(err, win.ERROR_ALREADY_EXISTS) {
					h, err = win.CreateFile(ptr, win.FILE_READ_ATTRIBUTES|win.READ_CONTROL, win.FILE_SHARE_READ|win.FILE_SHARE_WRITE, nil, win.OPEN_EXISTING, win.FILE_FLAG_BACKUP_SEMANTICS|win.FILE_FLAG_OPEN_REPARSE_POINT, 0)
				}
			}
		}
		if err != nil {
			closeGuards(guards)
			if errors.Is(err, win.ERROR_FILE_NOT_FOUND) || errors.Is(err, win.ERROR_PATH_NOT_FOUND) {
				return nil, fs.ErrNotExist
			}
			return nil, errUnsafe
		}
		guards = append(guards, h)
		// The local volume root is the immutable traversal trust anchor, as /
		// is on Linux. Its add-child permissions do not permit replacing an
		// existing component held open without share-delete.
		valid := true
		if i == 0 {
			var st win.ByHandleFileInformation
			valid = win.GetFileInformationByHandle(h, &st) == nil && st.FileAttributes&win.FILE_ATTRIBUTE_REPARSE_POINT == 0 && st.FileAttributes&win.FILE_ATTRIBUTE_DIRECTORY != 0
		} else {
			valid = f.validateHandle(h, private && i == len(paths)-1, true) == nil
		}
		if !valid {
			closeGuards(guards)
			return nil, errUnsafe
		}
	}
	return guards, nil
}
func (f *files) checked(path string, private, directory bool) error {
	if directory {
		g, e := f.guardPath(path, private, false)
		closeGuards(g)
		return e
	}
	root, name := filepath.Split(path)
	v, e := f.open(context.Background(), filepath.Clean(root), name, false, private)
	if e == nil {
		e = v.Close()
	}
	return e
}
func (f *files) PrivateState(ctx context.Context, path string) (string, error) {
	if ctx.Err() != nil {
		return "", ctx.Err()
	}
	g, e := f.guardPath(path, true, true)
	closeGuards(g)
	if e != nil {
		return "", e
	}
	return path, nil
}
func (f *files) EnsurePrivate(ctx context.Context, path string, directory bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return f.checked(path, true, directory)
}
func (f *files) OpenNoFollow(ctx context.Context, root, name string, create bool) (platform.File, error) {
	return f.open(ctx, root, name, create, true)
}
func (f *files) OpenProviderRecord(ctx context.Context, root, name string) (platform.File, error) {
	return f.open(ctx, root, name, false, false)
}
func (f *files) open(ctx context.Context, root, name string, create, private bool) (platform.File, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	if !validRelative(name) || !validPath(root) {
		return nil, errUnsafe
	}
	path := filepath.Join(root, name)
	rootGuards, e := f.guardPath(root, private, false)
	if e != nil {
		return nil, e
	}
	defer closeGuards(rootGuards)
	g, e := f.guardPath(filepath.Dir(path), private, false)
	if e != nil {
		return nil, e
	}
	ptr, _ := win.UTF16PtrFromString(path)
	access := uint32(win.GENERIC_READ | win.READ_CONTROL)
	disposition := uint32(win.OPEN_EXISTING)
	var sa *win.SecurityAttributes
	if create {
		access |= win.GENERIC_WRITE
		disposition = win.OPEN_ALWAYS
		sa = f.attributes()
	}
	sharing := uint32(win.FILE_SHARE_READ | win.FILE_SHARE_WRITE)
	if !private {
		sharing |= win.FILE_SHARE_DELETE
	}
	h, e := win.CreateFile(ptr, access, sharing, sa, disposition, win.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if e != nil {
		closeGuards(g)
		if errors.Is(e, win.ERROR_FILE_NOT_FOUND) {
			return nil, fs.ErrNotExist
		}
		return nil, errUnsafe
	}
	if f.validateHandle(h, private, false) != nil {
		win.CloseHandle(h)
		closeGuards(g)
		return nil, errUnsafe
	}
	if !private {
		// A provider may rotate records or rename their directories while a
		// reader holds this handle. Traversal was protected through open;
		// recordProvenance reopens the name and compares its file identity.
		closeGuards(g)
		g = nil
	}
	v := &verifiedFile{File: os.NewFile(uintptr(h), name), host: f, root: root, relative: name, guards: g}
	f.mu.Lock()
	f.opened[v] = true
	f.mu.Unlock()
	return v, nil
}
func (f *files) AtomicWrite(ctx context.Context, root, name string, data []byte) error {
	return f.atomicWrite(ctx, root, name, data, false)
}
func (f *files) AtomicWriteExclusive(ctx context.Context, root, name string, data []byte) error {
	return f.atomicWrite(ctx, root, name, data, true)
}
func (f *files) atomicWrite(ctx context.Context, root, name string, data []byte, exclusive bool) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !validRelative(name) {
		return errUnsafe
	}
	rootGuards, e := f.guardPath(root, true, false)
	if e != nil {
		return e
	}
	defer closeGuards(rootGuards)
	parent := filepath.Dir(filepath.Join(root, name))
	g, e := f.guardPath(parent, true, false)
	if e != nil {
		return e
	}
	defer closeGuards(g)
	old, e := f.OpenNoFollow(ctx, root, name, false)
	if e == nil {
		old.Close()
		if exclusive {
			return fs.ErrExist
		}
	} else if !errors.Is(e, fs.ErrNotExist) {
		return e
	}
	var random [16]byte
	if _, e = rand.Read(random[:]); e != nil {
		return errUnsafe
	}
	tmp := ".write-" + hex.EncodeToString(random[:])
	temp := filepath.Join(parent, tmp)
	ptr, _ := win.UTF16PtrFromString(temp)
	h, e := win.CreateFile(ptr, win.GENERIC_READ|win.GENERIC_WRITE|win.READ_CONTROL, 0, f.attributes(), win.CREATE_NEW, win.FILE_ATTRIBUTE_NORMAL, 0)
	if e != nil {
		return errUnsafe
	}
	out := os.NewFile(uintptr(h), tmp)
	_, e = out.Write(data)
	if e == nil {
		e = out.Sync()
	}
	ce := out.Close()
	if e == nil {
		e = ce
	}
	defer os.Remove(temp)
	if e != nil {
		return errors.New("local storage unavailable")
	}
	dest, _ := win.UTF16PtrFromString(filepath.Join(root, name))
	flags := uint32(win.MOVEFILE_WRITE_THROUGH)
	if !exclusive {
		flags |= win.MOVEFILE_REPLACE_EXISTING
	}
	if e = win.MoveFileEx(ptr, dest, flags); e != nil {
		if errors.Is(e, win.ERROR_ALREADY_EXISTS) || errors.Is(e, win.ERROR_FILE_EXISTS) {
			return fs.ErrExist
		}
		return errors.New("local storage unavailable")
	}
	// MoveFileEx WRITE_THROUGH persists the replacement; contents were flushed
	// before the move and its new protected DACL belongs to the temporary file.
	return nil
}
func (f *files) List(ctx context.Context, root string) ([]platform.Entry, error) {
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	g, e := f.guardPath(root, false, false)
	if e != nil {
		return nil, e
	}
	defer closeGuards(g)
	entries, e := os.ReadDir(root)
	if e != nil {
		return nil, errUnsafe
	}
	out := make([]platform.Entry, 0, len(entries))
	for _, v := range entries {
		if !validComponent(v.Name()) {
			return nil, errUnsafe
		}
		p := filepath.Join(root, v.Name())
		ptr, _ := win.UTF16PtrFromString(p)
		attrs, e := win.GetFileAttributes(ptr)
		if e != nil {
			return nil, errUnsafe
		}
		if attrs&win.FILE_ATTRIBUTE_REPARSE_POINT != 0 {
			return nil, errUnsafe
		}
		st, e := v.Info()
		if e != nil {
			return nil, errUnsafe
		}
		out = append(out, platform.Entry{Name: v.Name(), Size: st.Size(), Directory: v.IsDir()})
	}
	return out, nil
}
func (f *files) Remove(ctx context.Context, root, name string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if !validRelative(name) {
		return errUnsafe
	}
	path := filepath.Join(root, name)
	g, e := f.guardPath(filepath.Dir(path), true, false)
	if e != nil {
		return e
	}
	defer closeGuards(g)
	ptr, _ := win.UTF16PtrFromString(path)
	h, e := win.CreateFile(ptr, win.DELETE|win.FILE_READ_ATTRIBUTES|win.READ_CONTROL, win.FILE_SHARE_READ|win.FILE_SHARE_WRITE, nil, win.OPEN_EXISTING, win.FILE_FLAG_OPEN_REPARSE_POINT, 0)
	if e != nil {
		if errors.Is(e, win.ERROR_FILE_NOT_FOUND) {
			return fs.ErrNotExist
		}
		return errUnsafe
	}
	defer win.CloseHandle(h)
	if f.validateHandle(h, true, false) != nil {
		return errUnsafe
	}
	var disposition uint32 = 1
	if e = win.SetFileInformationByHandle(h, win.FileDispositionInfo, (*byte)(unsafe.Pointer(&disposition)), 4); e != nil {
		return errUnsafe
	}
	return nil
}

type lock struct {
	*verifiedFile
	overlap win.Overlapped
}

func (l *lock) Close() error {
	win.UnlockFileEx(win.Handle(l.Fd()), 0, 1, 0, &l.overlap)
	return l.verifiedFile.Close()
}
func (f *files) AdvisoryLock(ctx context.Context, root, name string, wait bool) (platform.Lock, error) {
	v, e := f.OpenNoFollow(ctx, root, name, true)
	if e != nil {
		return nil, e
	}
	l := &lock{verifiedFile: v.(*verifiedFile)}
	for {
		e = win.LockFileEx(win.Handle(l.Fd()), win.LOCKFILE_EXCLUSIVE_LOCK|win.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &l.overlap)
		if e == nil {
			return l, nil
		}
		if !wait || !errors.Is(e, win.ERROR_LOCK_VIOLATION) {
			l.verifiedFile.Close()
			return nil, errors.New("local lock unavailable")
		}
		select {
		case <-ctx.Done():
			l.verifiedFile.Close()
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}
func recordProvenance(v *verifiedFile) error {
	v.host.mu.Lock()
	live := v.host.opened[v]
	v.host.mu.Unlock()
	if !live {
		return errUnsafe
	}
	if v.host.validateHandle(win.Handle(v.Fd()), false, false) != nil {
		return errUnsafe
	}
	current, e := v.host.open(context.Background(), v.root, v.relative, false, false)
	if e != nil {
		return errUnsafe
	}
	defer current.Close()
	var a, b win.ByHandleFileInformation
	if win.GetFileInformationByHandle(win.Handle(v.Fd()), &a) != nil || win.GetFileInformationByHandle(win.Handle(current.(*verifiedFile).Fd()), &b) != nil || a.VolumeSerialNumber != b.VolumeSerialNumber || a.FileIndexHigh != b.FileIndexHigh || a.FileIndexLow != b.FileIndexLow {
		return errUnsafe
	}
	return nil
}
