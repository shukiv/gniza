package resticrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// largePlan writes what restic says about a repository nobody has
// thinned for three weeks: every snapshot named in full, and named again
// with the reason it is kept. It reports how many bytes it wrote.
func largePlan(t *testing.T, to interface{ Write([]byte) (int, error) }, accounts, each int) int {
	t.Helper()
	written := 0
	write := func(text string) {
		n, err := to.Write([]byte(text))
		if err != nil {
			t.Fatalf("write the plan: %v", err)
		}
		written += n
	}
	said := strings.Repeat("/home/customer/domains/example.com/public_html ", 40)
	write("[")
	for account := range accounts {
		if account > 0 {
			write(",")
		}
		write(fmt.Sprintf(`{"host":"uscp","tags":["account:c%d"],"keep":[`, account))
		for n := range each {
			if n > 0 {
				write(",")
			}
			write(fmt.Sprintf(`{"id":"%016x","time":"2026-09-%02dT02:00:00Z","tags":["account:c%d"],"paths":[%q]}`,
				account*1000+n, 1+n%28, account, said))
		}
		write(`],"remove":[`)
		for n := range each {
			if n > 0 {
				write(",")
			}
			write(fmt.Sprintf(`{"id":"%016x","time":"2026-08-%02dT02:00:00Z","tags":["account:c%d"],"paths":[%q]}`,
				account*1000+500+n, 1+n%28, account, said))
		}
		write(`],"reasons":[{"snapshot":{"paths":[` + fmt.Sprintf("%q", strings.Repeat(said, 20)) + `]},"matches":["daily snapshot"]}]}`)
	}
	write("]\n")
	return written
}

// On 182.54.236.10 the locks were removed and retention still could not
// run: the plan for 5908 snapshots was larger than the 8 MiB any
// command's output is captured under. A repository retention has not
// thinned is exactly the one with a large plan.
func TestAPlanLargerThanTheCaptureCapIsReadInFull(t *testing.T) {
	const accounts, each = 145, 20
	wrote := 0
	r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
		if cmd.Stdout == nil {
			t.Fatal("the plan was not asked for as a stream")
		}
		wrote = largePlan(t, cmd.Stdout, accounts, each)
		return CommandResult{}, nil
	}))
	plan, err := r.ForgetPlanned(t.Context(), completionRepo(t), ForgetSpec{KeepDaily: 7, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if wrote <= 8<<20 {
		t.Fatalf("the plan was only %d bytes, which the old cap held", wrote)
	}
	if len(plan.Groups) != accounts || plan.Kept != accounts*each || plan.Removed != accounts*each {
		t.Errorf("read %d groups, %d kept, %d removed", len(plan.Groups), plan.Kept, plan.Removed)
	}
}

// The same through a real process, whose output arrives through a pipe
// the reader has to keep up with.
func TestAPlanIsReadFromARunningProcess(t *testing.T) {
	dir := t.TempDir()
	plan, err := os.Create(filepath.Join(dir, "plan.json"))
	if err != nil {
		t.Fatal(err)
	}
	wrote := largePlan(t, plan, 60, 50)
	if err := plan.Close(); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "restic")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexec cat \"$(dirname \"$0\")/plan.json\"\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	r := New(Config{RuntimeDir: t.TempDir(), Binary: script}, &OSExec{})
	read, err := r.ForgetPlanned(t.Context(), completionRepo(t), ForgetSpec{KeepDaily: 7, DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if wrote <= 8<<20 {
		t.Fatalf("the plan was only %d bytes", wrote)
	}
	if len(read.Groups) != 60 || read.Removed != 60*50 {
		t.Errorf("read %d groups, %d removed", len(read.Groups), read.Removed)
	}
}

// What restic says when it could not run at all is still not a plan.
func TestARefusalOnTheStreamIsNotAnEmptyPlan(t *testing.T) {
	for said, want := range map[string]error{
		`{"message_type":"exit_error","code":11,"message":"repository is already locked"}`: ErrLocked,
		`{"message_type":"exit_error","code":1,"message":"wrong password"}`:                nil,
	} {
		r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
			_, _ = cmd.Stdout.Write([]byte(said + "\n"))
			return CommandResult{}, nil
		}))
		plan, err := r.ForgetPlanned(t.Context(), completionRepo(t), ForgetSpec{KeepDaily: 7, DryRun: true})
		if err == nil {
			t.Fatalf("%s was read as a plan: %+v", said, plan)
		}
		if want != nil && !errors.Is(err, want) {
			t.Errorf("%s: %v", said, err)
		}
	}

	// And one cut off in the middle is not a shorter plan.
	r := New(Config{RuntimeDir: t.TempDir()}, ExecFunc(func(_ context.Context, cmd Command) (CommandResult, error) {
		_, _ = cmd.Stdout.Write([]byte(`[{"host":"uscp","tags":["account:c1"],"keep":[{"id":"aaaaaaaaaaaaaaaa"}`))
		return CommandResult{}, nil
	}))
	if plan, err := r.ForgetPlanned(t.Context(), completionRepo(t), ForgetSpec{KeepDaily: 7, DryRun: true}); err == nil {
		t.Errorf("half a plan was read as a whole one: %+v", plan)
	}
}
