//go:build linux

package sandbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"runtime"
	"syscall"
	"unsafe"
)

// Landlock syscalls share these numbers on every Linux architecture.
const (
	sysLandlockCreateRuleset = 444
	sysLandlockAddRule       = 445
	sysLandlockRestrictSelf  = 446

	createRulesetVersion = 1 << 0

	ruleTypePathBeneath = 1
	ruleTypeNetPort     = 2

	prSetNoNewPrivs = 38

	// oPath is O_PATH on amd64 and arm64 (missing from package syscall).
	oPath = 0x200000
)

// Filesystem access rights (uapi/linux/landlock.h).
const (
	fsExecute    = 1 << 0
	fsWriteFile  = 1 << 1
	fsReadFile   = 1 << 2
	fsReadDir    = 1 << 3
	fsRemoveDir  = 1 << 4
	fsRemoveFile = 1 << 5
	fsMakeChar   = 1 << 6
	fsMakeDir    = 1 << 7
	fsMakeReg    = 1 << 8
	fsMakeSock   = 1 << 9
	fsMakeFifo   = 1 << 10
	fsMakeBlock  = 1 << 11
	fsMakeSym    = 1 << 12
	fsRefer      = 1 << 13 // ABI 2
	fsTruncate   = 1 << 14 // ABI 3
	fsIoctlDev   = 1 << 15 // ABI 5

	// fileRights are the only rights a rule on a non-directory may carry.
	fileRights = fsExecute | fsWriteFile | fsReadFile | fsTruncate | fsIoctlDev
)

// Network access rights (ABI 4) and scopes (ABI 6).
const (
	netBindTCP    = 1 << 0
	netConnectTCP = 1 << 1

	scopeAbstractUnixSocket = 1 << 0
	scopeSignal             = 1 << 1
)

// ABI returns the Landlock ABI version of the running kernel.
func ABI() (int, error) {
	v, _, errno := syscall.Syscall(sysLandlockCreateRuleset, 0, 0, createRulesetVersion)
	if errno != 0 {
		return 0, fmt.Errorf("%w: %v", ErrUnsupported, errno)
	}
	return int(v), nil
}

// handledFS is every filesystem right the given ABI knows about.
func handledFS(abi int) uint64 {
	h := uint64(fsMakeSym<<1 - 1) // ABI 1: execute .. make_sym
	if abi >= 2 {
		h |= fsRefer
	}
	if abi >= 3 {
		h |= fsTruncate
	}
	if abi >= 5 {
		h |= fsIoctlDev
	}
	return h
}

// Exec confines the calling process with rules and replaces it with argv. It
// only returns on failure. The process must not need its other threads again:
// Landlock restricts the calling thread, which is the one that execs.
func Exec(rules Rules, argv, env []string) error {
	if len(argv) == 0 {
		return errors.New("sandbox: empty argv")
	}
	abi, err := Check()
	if err != nil {
		return err
	}
	runtime.LockOSThread()
	// Never unlocked: on success the process image is replaced, on failure
	// the caller exits.

	fd, err := buildRuleset(abi, rules)
	if err != nil {
		return err
	}
	defer syscall.Close(fd)

	if _, _, errno := syscall.RawSyscall6(syscall.SYS_PRCTL, prSetNoNewPrivs, 1, 0, 0, 0, 0); errno != 0 {
		return fmt.Errorf("sandbox: no_new_privs: %v", errno)
	}
	if _, _, errno := syscall.RawSyscall(sysLandlockRestrictSelf, uintptr(fd), 0, 0); errno != 0 {
		return fmt.Errorf("sandbox: restrict_self: %v", errno)
	}
	return syscall.Exec(argv[0], argv, env)
}

// buildRuleset creates a ruleset handling every right the ABI supports and
// grants exactly what rules allow.
func buildRuleset(abi int, rules Rules) (int, error) {
	fs := handledFS(abi)
	attr := make([]byte, 24)
	binary.NativeEndian.PutUint64(attr[0:], fs)
	binary.NativeEndian.PutUint64(attr[8:], netBindTCP|netConnectTCP)
	size := 16
	if abi >= 6 {
		binary.NativeEndian.PutUint64(attr[16:], scopeAbstractUnixSocket|scopeSignal)
		size = 24
	}
	r, _, errno := syscall.Syscall(sysLandlockCreateRuleset, uintptr(unsafe.Pointer(&attr[0])), uintptr(size), 0)
	runtime.KeepAlive(attr)
	if errno != 0 {
		return -1, fmt.Errorf("sandbox: create_ruleset: %v", errno)
	}
	fd := int(r)

	read := uint64(fsReadFile | fsReadDir)
	grants := []struct {
		paths  []string
		access uint64
	}{
		{rules.ReadOnly, read},
		{rules.Exec, read | fsExecute},
		{rules.ReadWrite, fs &^ fsExecute},
	}
	for _, g := range grants {
		for _, p := range g.paths {
			if err := addPath(fd, p, g.access&fs); err != nil {
				syscall.Close(fd)
				return -1, err
			}
		}
	}
	for _, port := range rules.ConnectTCP {
		if err := addPort(fd, port); err != nil {
			syscall.Close(fd)
			return -1, err
		}
	}
	return fd, nil
}

func addPath(rulesetFD int, path string, access uint64) error {
	f, err := os.OpenFile(path, oPath|syscall.O_CLOEXEC, 0)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("sandbox: open %s: %w", path, err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("sandbox: stat %s: %w", path, err)
	}
	if !fi.IsDir() {
		access &= fileRights
	}
	// struct landlock_path_beneath_attr is packed: u64 allowed_access, s32 parent_fd.
	attr := make([]byte, 12)
	binary.NativeEndian.PutUint64(attr[0:], access)
	binary.NativeEndian.PutUint32(attr[8:], uint32(f.Fd()))
	_, _, errno := syscall.Syscall6(sysLandlockAddRule, uintptr(rulesetFD), ruleTypePathBeneath,
		uintptr(unsafe.Pointer(&attr[0])), 0, 0, 0)
	runtime.KeepAlive(attr)
	runtime.KeepAlive(f)
	if errno != 0 {
		return fmt.Errorf("sandbox: add rule for %s: %v", path, errno)
	}
	return nil
}

func addPort(rulesetFD int, port uint16) error {
	// struct landlock_net_port_attr: u64 allowed_access, u64 port.
	attr := make([]byte, 16)
	binary.NativeEndian.PutUint64(attr[0:], netConnectTCP)
	binary.NativeEndian.PutUint64(attr[8:], uint64(port))
	_, _, errno := syscall.Syscall6(sysLandlockAddRule, uintptr(rulesetFD), ruleTypeNetPort,
		uintptr(unsafe.Pointer(&attr[0])), 0, 0, 0)
	runtime.KeepAlive(attr)
	if errno != 0 {
		return fmt.Errorf("sandbox: add rule for tcp port %d: %v", port, errno)
	}
	return nil
}
