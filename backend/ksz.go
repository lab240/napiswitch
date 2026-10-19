package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// KSZ switch registers, read through the ksz driver's regmap in debugfs
// (read-only: the driver owns the chip, writes go through kmod/kszreg).

// kszRegmap finds the 8-bit regmap of the ksz switch: "<spidev>-8" whose
// name file reads "ksz-switch".
func kszRegmap(regmaps string) (dir, dev string, err error) {
	ents, err := filepath.Glob(filepath.Join(regmaps, "*-8"))
	if err != nil {
		return "", "", err
	}
	for _, d := range ents {
		if b, err := os.ReadFile(filepath.Join(d, "name")); err == nil && strings.TrimSpace(string(b)) == "ksz-switch" {
			return d, strings.TrimSuffix(filepath.Base(d), "-8"), nil
		}
	}
	return "", "", errors.New("ksz regmap not found in debugfs (is debugfs mounted?)")
}

// readReg reads one register from a regmap debugfs "registers" file. Lines
// have a fixed width ("006301: 41\n"), so the register's line is found by
// offset instead of scanning the whole 16M-register map.
func readReg(dir string, reg int) (int, error) {
	fh, err := os.Open(filepath.Join(dir, "registers"))
	if err != nil {
		return 0, err
	}
	defer fh.Close()
	first := make([]byte, 32)
	n, _ := fh.ReadAt(first, 0)
	i := bytes.IndexByte(first[:n], '\n')
	if i < 0 {
		return 0, errors.New("regmap registers: unexpected format")
	}
	width := i + 1
	line := make([]byte, width)
	if _, err := fh.ReadAt(line, int64(reg*width)); err != nil {
		return 0, fmt.Errorf("read reg 0x%x: %v", reg, err)
	}
	addr, val, ok := strings.Cut(strings.TrimSpace(string(line)), ": ")
	if a, err := strconv.ParseInt(addr, 16, 32); !ok || err != nil || int(a) != reg {
		return 0, fmt.Errorf("regmap registers: line for 0x%x is %q", reg, line)
	}
	v, err := strconv.ParseUint(val, 16, 8)
	return int(v), err
}
