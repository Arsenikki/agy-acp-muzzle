// Package sandbox confines a process with Landlock before it execs the kernel.
//
// Landlock is an unprivileged Linux security module: a process restricts
// itself, and the restriction is inherited by everything it execs or forks and
// cannot be lifted. The muzzle applies it in a short-lived launcher (a re-exec
// of itself) that then execs the kernel, so the muzzle process itself stays
// unrestricted.
package sandbox

import (
	"errors"
	"fmt"
)

// Rules describes what the confined process may do. Paths that do not exist
// are skipped. Everything not granted is denied.
type Rules struct {
	// Exec paths may be executed and read (directories: everything beneath).
	Exec []string `json:"exec,omitempty"`
	// ReadOnly paths may be read.
	ReadOnly []string `json:"readOnly,omitempty"`
	// ReadWrite paths may be read, written, created and removed.
	ReadWrite []string `json:"readWrite,omitempty"`
	// ConnectTCP lists the only TCP ports the process may connect to. Binding
	// a TCP port is always denied. UDP is not covered by Landlock.
	ConnectTCP []uint16 `json:"connectTCP,omitempty"`
}

// ErrUnsupported means the running kernel cannot enforce the rules.
var ErrUnsupported = errors.New("landlock is not supported")

// MinABI is the lowest Landlock ABI that enforces all of Rules: version 4 adds
// TCP connect/bind control.
const MinABI = 4

// Check reports whether the running kernel can enforce Rules.
func Check() (int, error) {
	abi, err := ABI()
	if err != nil {
		return 0, err
	}
	if abi < MinABI {
		return abi, fmt.Errorf("%w: ABI %d, need %d", ErrUnsupported, abi, MinABI)
	}
	return abi, nil
}
