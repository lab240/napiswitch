package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Cmd is one step of an apply plan: a system command, or writing/removing
// a file (File set; Content is written, or the file removed if Remove).
type Cmd struct {
	Args      []string `json:"args"`
	IgnoreErr bool     `json:"ignore_error,omitempty"`
	File      string   `json:"file,omitempty"`
	Content   string   `json:"content,omitempty"`
	Remove    bool     `json:"remove,omitempty"`
}

func cmd(args ...string) Cmd               { return Cmd{Args: args} }
func cmdIgn(args ...string) Cmd            { return Cmd{Args: args, IgnoreErr: true} }
func writeFile(path, content string) Cmd   { return Cmd{File: path, Content: content} }
func removeFile(path string) Cmd           { return Cmd{File: path, Remove: true} }
func (c Cmd) MarshalText() ([]byte, error) { return []byte(c.String()), nil }

func (c Cmd) String() string {
	switch {
	case c.File != "" && c.Remove:
		return "rm -f " + shellJoin([]string{c.File})
	case c.File != "":
		return "write " + shellJoin([]string{c.File})
	}
	return shellJoin(c.Args)
}

// Runner executes system tools. Output is for read-only queries, Exec for changes.
type Runner interface {
	Output(args ...string) ([]byte, error)
	Exec(c Cmd) error
}

type sysRunner struct {
	log *log.Logger
	dry bool
}

func (r *sysRunner) Output(args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, args[0], args[1:]...)
	var stderr bytes.Buffer
	c.Stderr = &stderr
	out, err := c.Output()
	if err != nil {
		return out, fmt.Errorf("%s: %v: %s", shellJoin(args), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

func (r *sysRunner) Exec(c Cmd) error {
	if r.dry {
		r.log.Printf("dry-run: %s", c)
		return nil
	}
	r.log.Printf("exec: %s", c)
	if c.File != "" {
		return applyFile(c)
	}
	if _, err := r.Output(c.Args...); err != nil {
		if c.IgnoreErr {
			r.log.Printf("ignored: %v", err)
			return nil
		}
		return err
	}
	return nil
}

func shellJoin(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		if a == "" || strings.ContainsAny(a, " \t'\"\\$`") {
			a = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
		}
		q[i] = a
	}
	return strings.Join(q, " ")
}

func applyFile(c Cmd) error {
	if c.Remove {
		if err := os.Remove(c.File); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		return nil
	}
	return writeAtomic(c.File, []byte(c.Content), 0o644)
}
