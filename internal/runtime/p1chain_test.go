package runtime

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// TestWriteRefusesSymlinkedParentComponent (#P1): an agent plants an internal symlink DIRECTORY
// under .vichu (workers -> victim, both inside the project); a later kernel write under it must
// NOT be redirected onto the victim. os.Root follows an internal link, so safeio alone guards only
// the final component — the runtime's confine walks the whole chain and refuses a planted link.
func TestWriteRefusesSymlinkedParentComponent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-1", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	runDir := s.RunDir("run-1")
	// A user file the agent wants the kernel to clobber, and workers -> victim (internal).
	if err := os.MkdirAll(filepath.Join(runDir, "victim"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "victim", "status.json"), []byte("USER_CANARY"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("victim", filepath.Join(runDir, "workers")); err != nil {
		t.Fatal(err)
	}

	err := s.SaveWorkerStatus("run-1", &core.WorkerStatus{ID: "explore-01", Stage: "explore", Status: core.WorkerRunning})
	if err == nil {
		t.Fatal("SaveWorkerStatus wrote through a planted symlinked 'workers' directory")
	}
	if data, _ := os.ReadFile(filepath.Join(runDir, "victim", "status.json")); string(data) != "USER_CANARY" {
		t.Fatalf("the kernel overwrote a file through a symlinked parent component: %q", data)
	}
}
