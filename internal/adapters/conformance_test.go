package adapters

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// runConformance exercises the adapter contract: probe, start, drain events,
// fetch result, and either resume or declare resume unsupported. Every adapter
// must pass it.
func runConformance(t *testing.T, a Adapter, inv Invocation) {
	t.Helper()
	ctx := context.Background()

	avail, err := a.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}
	if !avail.Available {
		t.Fatalf("adapter %s reported unavailable: %s", a.Name(), avail.Reason)
	}

	sess, err := a.Start(ctx, inv)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var sawDone bool
	for ev := range sess.Events() {
		if ev.Kind == EventDone {
			sawDone = true
		}
	}
	if !sawDone {
		t.Errorf("adapter %s never emitted a done event", a.Name())
	}
	if _, err := sess.Result(ctx); err != nil {
		t.Fatalf("Result: %v", err)
	}

	// Resume must either work or be cleanly declared unsupported.
	caps := a.Capabilities()
	rsess, rerr := a.Resume(ctx, "some-session", inv)
	if caps.Resume && rerr != nil {
		t.Errorf("adapter %s claims Resume but Resume errored: %v", a.Name(), rerr)
	}
	if !caps.Resume && rerr != ErrResumeUnsupported {
		t.Errorf("adapter %s lacks Resume but did not return ErrResumeUnsupported: %v", a.Name(), rerr)
	}
	// Fully drain the resumed session before returning: its background goroutine
	// writes files into inv.WorkDir, and abandoning it mid-write races with the
	// test's TempDir cleanup ("unlinkat: directory not empty" on Linux/-race).
	// Draining also confirms the resumed run reaches a done event.
	if rsess != nil {
		var resumeDone bool
		for ev := range rsess.Events() {
			if ev.Kind == EventDone {
				resumeDone = true
			}
		}
		if caps.Resume && !resumeDone {
			t.Errorf("adapter %s resumed but never emitted a done event", a.Name())
		}
		_, _ = rsess.Result(ctx)
	}
}

func TestFakeConformance(t *testing.T) {
	dir := t.TempDir()
	fake := NewFake(FakeScript{
		ResultText: "done",
		Actions: map[string][]FakeAction{
			"implementer": {{Type: "write_file", Path: "out.txt", Content: "hi"}},
		},
	})
	runConformance(t, fake, Invocation{Role: "implementer", WorkDir: dir})

	if _, err := os.Stat(filepath.Join(dir, "out.txt")); err != nil {
		t.Fatalf("fake action did not write file: %v", err)
	}
}

func TestShellConformance(t *testing.T) {
	cmd := []string{"sh", "-c", "echo hello"}
	if runtime.GOOS == "windows" {
		cmd = []string{"cmd", "/c", "echo hello"}
	}
	runConformance(t, NewShell(), Invocation{Role: "implementer", WorkDir: t.TempDir(), Command: cmd})
}

func TestShellCapturesOutputAndExit(t *testing.T) {
	cmd := []string{"sh", "-c", "echo line1; echo line2"}
	if runtime.GOOS == "windows" {
		cmd = []string{"cmd", "/c", "echo line1& echo line2"}
	}
	sess, err := NewShell().Start(context.Background(), Invocation{WorkDir: t.TempDir(), Command: cmd})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	var lines int
	for ev := range sess.Events() {
		if ev.Kind == EventText {
			lines++
		}
	}
	if lines < 2 {
		t.Fatalf("want >=2 text lines, got %d", lines)
	}
	res, err := sess.Result(context.Background())
	if err != nil {
		t.Fatalf("Result: %v", err)
	}
	if res.Data["exit_code"] != 0 {
		t.Fatalf("want exit_code 0, got %v", res.Data["exit_code"])
	}
}

func TestShellRequiresCommand(t *testing.T) {
	if _, err := NewShell().Start(context.Background(), Invocation{WorkDir: t.TempDir()}); err == nil {
		t.Fatal("expected error when no command provided")
	}
}
