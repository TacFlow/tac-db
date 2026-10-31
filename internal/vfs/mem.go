package vfs

import (
	"io"
	"math/rand/v2"
	"path"
	"sort"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/TacFlow/tacdb/internal/errs"
)

// MemFS is an in-memory FS for tests (SPEC §8.3). Every file and directory
// has a current image, which reads see, and a durable image, which is what
// survives Crash. Sync promotes a file; SyncDir promotes the entries of a
// directory. Faults are deterministic.
type MemFS struct {
	mu       sync.Mutex
	device   uint64
	nextID   uint64
	root     *memNode
	faults   []*Fault
	calls    map[faultKey]int
	capacity int64 // 0 = unlimited
	used     int64
	epoch    uint64 // incremented by Crash; older handles are dead
}

var memDevices atomic.Uint64

// NewMemFS returns an empty in-memory directory.
func NewMemFS() *MemFS {
	m := &MemFS{device: memDevices.Add(1), calls: make(map[faultKey]int)}
	m.root = m.newNode(true)
	return m
}

type memNode struct {
	id    uint64
	isDir bool
	// files
	cur, dur []byte
	pending  []memOp // operations since the last Sync, in order
	locks    map[*memFile]LockMode
	// directories
	curEntries, durEntries map[string]*memNode
}

type memOp struct {
	off      int64
	data     []byte // nil for a truncate
	truncate int64
}

func (m *MemFS) newNode(isDir bool) *memNode {
	m.nextID++
	n := &memNode{id: m.nextID, isDir: isDir}
	if isDir {
		n.curEntries = make(map[string]*memNode)
		n.durEntries = make(map[string]*memNode)
	} else {
		n.locks = make(map[*memFile]LockMode)
	}
	return n
}

// Op names an operation for fault injection.
type Op string

// Operations that faults can target.
const (
	OpCreate  Op = "create"
	OpOpen    Op = "open"
	OpRead    Op = "read"
	OpWrite   Op = "write"
	OpSync    Op = "sync"
	OpSyncDir Op = "syncdir"
	OpRename  Op = "rename"
	OpRemove  Op = "remove"
	OpTrunc   Op = "truncate"
)

// Fault makes the Nth matching call (1-based; 0 = every call) of Op fail.
// Name is a path.Match pattern ("db.tacdb-wal/*"); "" matches any name. A
// ShortWrite fault writes half of the buffer before failing. Calls are
// counted per Op and name.
type Fault struct {
	Op         Op
	Name       string
	Nth        int
	ShortWrite bool
}

type faultKey struct {
	op   Op
	name string
}

// Inject adds a fault.
func (m *MemFS) Inject(f Fault) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.faults = append(m.faults, &f)
}

// SetCapacity limits the total bytes of all files; writes beyond it fail
// with NO_SPACE. Zero removes the limit.
func (m *MemFS) SetCapacity(bytes int64) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.capacity = bytes
}

// fault reports the fault that applies to this call, if any. m.mu is held.
func (m *MemFS) fault(op Op, name string) *Fault {
	k := faultKey{op, name}
	m.calls[k]++
	n := m.calls[k]
	for _, f := range m.faults {
		if f.Op != op || (f.Name != "" && !matchName(f.Name, name)) {
			continue
		}
		if f.Nth == 0 || f.Nth == n {
			return f
		}
	}
	return nil
}

func matchName(pattern, name string) bool {
	ok, err := path.Match(pattern, name)
	return err == nil && ok
}

func injected(op Op, name string) error {
	return errs.Newf(errs.CodeIO, "vfs.mem."+string(op), "%q: injected fault", name)
}

// lookup resolves a valid name in the current namespace. m.mu is held.
func (m *MemFS) lookup(name string) (*memNode, *memNode, string, error) {
	if name == "." {
		return m.root, nil, "", nil
	}
	dir := m.root
	parts := strings.Split(name, "/")
	if len(parts) == 2 {
		d, ok := m.root.curEntries[parts[0]]
		if !ok || !d.isDir {
			return nil, nil, "", errs.Newf(errs.CodeNotFound, "vfs.mem", "%q: parent directory not found", name)
		}
		dir = d
	}
	leaf := parts[len(parts)-1]
	return dir.curEntries[leaf], dir, leaf, nil
}

// CrashOptions selects what survives a simulated power loss beyond the
// durable image.
type CrashOptions struct {
	Seed uint64
	// TornWrites applies a random subset of the unsynced writes, each
	// possibly cut at a 512-byte sector boundary.
	TornWrites bool
	// Reorder applies the surviving unsynced writes in random order.
	Reorder bool
	// Truncate cuts the named files, after the crash, to the given size
	// when they are longer, as a torn tail or an external truncation
	// would. A negative size cuts to zero.
	Truncate map[string]int64
}

// Crash simulates a power loss: every handle dies, every lock is released
// and every file and directory reverts to its durable image, possibly
// with some unsynced writes applied according to opts.
func (m *MemFS) Crash(opts CrashOptions) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r := rand.New(rand.NewPCG(opts.Seed, 1))
	m.epoch++
	var walk func(n *memNode)
	walk = func(n *memNode) {
		if n.isDir {
			n.curEntries = copyEntries(n.durEntries)
			for _, c := range n.durEntries {
				walk(c)
			}
			return
		}
		img := append([]byte(nil), n.dur...)
		if opts.TornWrites || opts.Reorder {
			ops := append([]memOp(nil), n.pending...)
			if opts.Reorder {
				r.Shuffle(len(ops), func(i, j int) { ops[i], ops[j] = ops[j], ops[i] })
			}
			for _, op := range ops {
				if opts.TornWrites && r.IntN(2) == 0 {
					continue
				}
				img = applyOp(img, op, opts.TornWrites, r)
			}
		}
		n.cur, n.dur, n.pending = img, append([]byte(nil), img...), nil
		n.locks = make(map[*memFile]LockMode)
	}
	walk(m.root)
	for name, size := range opts.Truncate {
		size = max(size, 0)
		if n, _, _, err := m.lookup(name); err == nil && n != nil && !n.isDir && int64(len(n.dur)) > size {
			n.dur, n.cur = n.dur[:size], append([]byte(nil), n.dur[:size]...)
		}
	}
	m.used = 0
	var count func(n *memNode)
	count = func(n *memNode) {
		for _, c := range n.curEntries {
			if c.isDir {
				count(c)
			} else {
				m.used += int64(len(c.cur))
			}
		}
	}
	count(m.root)
}

func applyOp(img []byte, op memOp, torn bool, r *rand.Rand) []byte {
	if op.data == nil {
		return resize(img, op.truncate)
	}
	data := op.data
	if torn {
		sectors := (len(data) + 511) / 512
		keep := r.IntN(sectors + 1)
		if keep*512 < len(data) {
			data = data[:keep*512]
		}
	}
	end := op.off + int64(len(data))
	if int64(len(img)) < end {
		img = resize(img, end)
	}
	copy(img[op.off:], data)
	return img
}

func resize(b []byte, size int64) []byte {
	if int64(len(b)) >= size {
		return b[:size]
	}
	return append(b, make([]byte, size-int64(len(b)))...)
}

func copyEntries(src map[string]*memNode) map[string]*memNode {
	dst := make(map[string]*memNode, len(src))
	for k, v := range src {
		dst[k] = v
	}
	return dst
}

// Create implements FS.
func (m *MemFS) Create(name string) (File, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault(OpCreate, name) != nil {
		return nil, injected(OpCreate, name)
	}
	n, dir, leaf, err := m.lookup(name)
	if err != nil {
		return nil, err
	}
	if n != nil {
		return nil, errs.Newf(errs.CodeDestinationExists, "vfs.mem.create", "%q exists", name)
	}
	n = m.newNode(false)
	dir.curEntries[leaf] = n
	return m.handle(n, name, false), nil
}

// Open implements FS.
func (m *MemFS) Open(name string) (File, error) { return m.open(name, false) }

// OpenReadOnly implements FS.
func (m *MemFS) OpenReadOnly(name string) (File, error) { return m.open(name, true) }

func (m *MemFS) open(name string, readOnly bool) (File, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault(OpOpen, name) != nil {
		return nil, injected(OpOpen, name)
	}
	n, _, _, err := m.lookup(name)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, errs.Newf(errs.CodeNotFound, "vfs.mem.open", "%q not found", name)
	}
	if n.isDir {
		return nil, errs.Newf(errs.CodeUnsafePath, "vfs.mem.open", "%q is a directory", name)
	}
	return m.handle(n, name, readOnly), nil
}

func (m *MemFS) handle(n *memNode, name string, readOnly bool) *memFile {
	return &memFile{fs: m, node: n, name: name, readOnly: readOnly, epoch: m.epoch}
}

// Mkdir implements FS.
func (m *MemFS) Mkdir(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	if strings.Contains(name, "/") {
		return errs.Newf(errs.CodeUnsafePath, "vfs.mem.mkdir", "%q: nested directories are not supported", name)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.root.curEntries[name]; ok {
		return errs.Newf(errs.CodeDestinationExists, "vfs.mem.mkdir", "%q exists", name)
	}
	m.root.curEntries[name] = m.newNode(true)
	return nil
}

// Remove implements FS.
func (m *MemFS) Remove(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault(OpRemove, name) != nil {
		return injected(OpRemove, name)
	}
	n, dir, leaf, err := m.lookup(name)
	if err != nil {
		return err
	}
	if n == nil {
		return errs.Newf(errs.CodeNotFound, "vfs.mem.remove", "%q not found", name)
	}
	if n.isDir && len(n.curEntries) > 0 {
		return errs.Newf(errs.CodeIO, "vfs.mem.remove", "%q is not empty", name)
	}
	if !n.isDir {
		m.used -= int64(len(n.cur))
	}
	delete(dir.curEntries, leaf)
	return nil
}

// Rename implements FS.
func (m *MemFS) Rename(oldName, newName string) error {
	if err := ValidName(oldName); err != nil {
		return err
	}
	if err := ValidName(newName); err != nil {
		return err
	}
	if dirOf(oldName) != dirOf(newName) {
		return errs.Newf(errs.CodeInvalidArgument, "vfs.mem.rename", "%q and %q are in different directories", oldName, newName)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault(OpRename, oldName) != nil {
		return injected(OpRename, oldName)
	}
	n, dir, oldLeaf, err := m.lookup(oldName)
	if err != nil {
		return err
	}
	if n == nil {
		return errs.Newf(errs.CodeNotFound, "vfs.mem.rename", "%q not found", oldName)
	}
	_, _, newLeaf, _ := m.lookup(newName)
	if old := dir.curEntries[newLeaf]; old != nil && !old.isDir {
		m.used -= int64(len(old.cur))
	}
	dir.curEntries[newLeaf] = n
	delete(dir.curEntries, oldLeaf)
	return nil
}

// SyncDir implements FS.
func (m *MemFS) SyncDir(name string) error {
	if err := ValidName(name); err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fault(OpSyncDir, name) != nil {
		return injected(OpSyncDir, name)
	}
	n, _, _, err := m.lookup(name)
	if err != nil {
		return err
	}
	if n == nil || !n.isDir {
		return errs.Newf(errs.CodeNotFound, "vfs.mem.syncdir", "%q is not a directory", name)
	}
	n.durEntries = copyEntries(n.curEntries)
	return nil
}

// List implements FS.
func (m *MemFS) List(name string) ([]string, error) {
	if err := ValidName(name); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, _, _, err := m.lookup(name)
	if err != nil {
		return nil, err
	}
	if n == nil || !n.isDir {
		return nil, errs.Newf(errs.CodeNotFound, "vfs.mem.list", "%q is not a directory", name)
	}
	names := make([]string, 0, len(n.curEntries))
	for k := range n.curEntries {
		names = append(names, k)
	}
	sort.Strings(names)
	return names, nil
}

// Exists implements FS.
func (m *MemFS) Exists(name string) (bool, error) {
	if err := ValidName(name); err != nil {
		return false, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	n, _, _, err := m.lookup(name)
	if errs.CodeOf(err) == errs.CodeNotFound {
		return false, nil
	}
	return n != nil, err
}

// Close implements FS.
func (m *MemFS) Close() error { return nil }

// ReadDurable returns a copy of the durable image of a file, for tests.
func (m *MemFS) ReadDurable(name string) ([]byte, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n, _, _, err := m.lookup(name)
	if err != nil || n == nil || n.isDir {
		return nil, false
	}
	return append([]byte(nil), n.dur...), true
}

type memFile struct {
	fs       *MemFS
	node     *memNode
	name     string
	readOnly bool
	epoch    uint64
	closed   bool
}

// check is called with fs.mu held.
func (f *memFile) check(op string) error {
	if f.closed || f.epoch != f.fs.epoch {
		return errs.Newf(errs.CodeClosed, "vfs.mem."+op, "%q: handle closed", f.name)
	}
	return nil
}

func (f *memFile) ReadAt(p []byte, off int64) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.check("read"); err != nil {
		return err
	}
	if f.fs.fault(OpRead, f.name) != nil {
		return injected(OpRead, f.name)
	}
	if off < 0 || off+int64(len(p)) > int64(len(f.node.cur)) {
		return errs.Wrap(errs.CodeIO, "vfs.mem.read", io.ErrUnexpectedEOF, f.name)
	}
	copy(p, f.node.cur[off:])
	return nil
}

func (f *memFile) WriteAt(p []byte, off int64) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.check("write"); err != nil {
		return err
	}
	if f.readOnly {
		return errs.Newf(errs.CodeReadOnly, "vfs.mem.write", "%q opened read-only", f.name)
	}
	if off < 0 {
		return errs.Newf(errs.CodeInvalidArgument, "vfs.mem.write", "negative offset %d", off)
	}
	data := p
	fault := f.fs.fault(OpWrite, f.name)
	if fault != nil && !fault.ShortWrite {
		return injected(OpWrite, f.name)
	}
	if fault != nil {
		data = p[:len(p)/2]
	}
	end := off + int64(len(data))
	if grow := end - int64(len(f.node.cur)); grow > 0 && f.fs.capacity > 0 && f.fs.used+grow > f.fs.capacity {
		return errs.Newf(errs.CodeNoSpace, "vfs.mem.write", "%q: no space left", f.name).
			With("capacity", f.fs.capacity)
	}
	if grow := end - int64(len(f.node.cur)); grow > 0 {
		f.fs.used += grow
		f.node.cur = resize(f.node.cur, end)
	}
	copy(f.node.cur[off:], data)
	f.node.pending = append(f.node.pending, memOp{off: off, data: append([]byte(nil), data...)})
	if fault != nil {
		return errs.Newf(errs.CodeIO, "vfs.mem.write", "%q: short write (%d of %d bytes)", f.name, len(data), len(p))
	}
	return nil
}

func (f *memFile) Size() (int64, error) {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.check("size"); err != nil {
		return 0, err
	}
	return int64(len(f.node.cur)), nil
}

func (f *memFile) Truncate(size int64) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.check("truncate"); err != nil {
		return err
	}
	if f.readOnly {
		return errs.Newf(errs.CodeReadOnly, "vfs.mem.truncate", "%q opened read-only", f.name)
	}
	if size < 0 {
		return errs.Newf(errs.CodeInvalidArgument, "vfs.mem.truncate", "negative size %d", size)
	}
	if f.fs.fault(OpTrunc, f.name) != nil {
		return injected(OpTrunc, f.name)
	}
	f.fs.used += size - int64(len(f.node.cur))
	f.node.cur = resize(f.node.cur, size)
	f.node.pending = append(f.node.pending, memOp{truncate: size})
	return nil
}

func (f *memFile) Preallocate(int64) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	return f.check("preallocate")
}

func (f *memFile) Sync() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.check("sync"); err != nil {
		return err
	}
	if f.fs.fault(OpSync, f.name) != nil {
		return injected(OpSync, f.name)
	}
	f.node.dur = append([]byte(nil), f.node.cur...)
	f.node.pending = nil
	return nil
}

func (f *memFile) SyncData() error { return f.Sync() }

func (f *memFile) Lock(mode LockMode) error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.check("lock"); err != nil {
		return err
	}
	for holder, held := range f.node.locks {
		if holder != f && (mode == LockExclusive || held == LockExclusive) {
			return errs.Newf(errs.CodeLocked, "vfs.mem.lock", "%q is locked", f.name)
		}
	}
	f.node.locks[f] = mode
	return nil
}

func (f *memFile) Unlock() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if err := f.check("unlock"); err != nil {
		return err
	}
	delete(f.node.locks, f)
	return nil
}

func (f *memFile) ID() (FileID, error) {
	return FileID{Device: f.fs.device, Inode: f.node.id}, nil
}

func (f *memFile) Close() error {
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	if f.closed {
		return nil
	}
	f.closed = true
	if f.epoch == f.fs.epoch {
		delete(f.node.locks, f)
	}
	return nil
}
