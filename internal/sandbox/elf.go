package sandbox

import (
	"bytes"
	"debug/elf"
	"io"
	"os"
	"strings"
)

// Interpreter returns the dynamic loader an ELF executable names in its
// PT_INTERP header, or "" for a static binary or a non-ELF file (e.g. a
// script). Executing a dynamic binary requires exec rights on its loader.
func Interpreter(path string) (string, error) {
	magic := make([]byte, len(elf.ELFMAG))
	fh, err := os.Open(path)
	if err != nil {
		return "", err
	}
	_, err = io.ReadFull(fh, magic)
	fh.Close()
	if err != nil || !bytes.Equal(magic, []byte(elf.ELFMAG)) {
		return "", nil
	}

	f, err := elf.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	for _, p := range f.Progs {
		if p.Type != elf.PT_INTERP {
			continue
		}
		b, err := io.ReadAll(p.Open())
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\x00"), nil
	}
	return "", nil
}
