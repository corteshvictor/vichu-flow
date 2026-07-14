package runtime

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// TestStoreWritesAreConfinedToVichu is the regression for the runtime's half of the
// symlink-follow bug class: the workspace layer was hardened with safeio, the Store was not.
// Every Store write goes through a confined root now, so a symlink an agent plants under
// .vichu cannot redirect the write to an external file.
func TestStoreWritesAreConfinedToVichu(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	project := t.TempDir()
	s := Open(project)

	st := &core.State{RunID: "run-1", Status: core.StatusActive}
	if err := s.CreateRun(st); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}

	// A file outside the project that nothing may touch, and the agent plants state.json as a
	// symlink to it (it knows the run id — it is in .vichu/runs/).
	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("ORIGINAL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(s.RunDir("run-1"), "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, statePath); err != nil {
		t.Fatal(err)
	}

	// The kernel saves state. It must NOT write through the planted link.
	st.Status = core.StatusCompleted
	err := s.SaveState(st)

	if data, _ := os.ReadFile(outside); string(data) != "ORIGINAL\n" {
		t.Fatalf("SaveState wrote through a planted symlink to an external file: %q", data)
	}
	// Either the write is refused, or the atomic rename replaced the link with a real file —
	// both are safe. What must never happen is the external file changing, asserted above.
	if err == nil {
		fi, lerr := os.Lstat(statePath)
		if lerr != nil || fi.Mode()&os.ModeSymlink != 0 {
			t.Fatalf("state.json is still a symlink after a successful save (%v)", lerr)
		}
	}
}

// TestLoadStateRefusesSymlinkedStateJSON is the READ half of the confinement guarantee (the
// write half is above): plain os.ReadFile followed a symlink an agent planted under .vichu, so a
// state.json → external-file link had that file read back as the run's own state. Confined,
// no-follow reads refuse it.
func TestLoadStateRefusesSymlinkedStateJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-read", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.json")
	if err := os.WriteFile(external, []byte(`{"run_id":"run-read","current_stage":"EXTERNAL_JSON_CANARY"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	statePath := filepath.Join(s.RunDir("run-read"), "state.json")
	if err := os.Remove(statePath); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, statePath); err != nil {
		t.Fatal(err)
	}

	st, err := s.LoadState("run-read")
	if err == nil {
		t.Fatalf("LoadState followed a symlinked state.json and read external content: current_stage=%q", st.CurrentStage)
	}
}

// TestReadEventsRefusesSymlinkedLog: the audit timeline read must be confined too, or `status`
// would show an external file's lines as the run's events.
func TestReadEventsRefusesSymlinkedLog(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-ev", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.ndjson")
	if err := os.WriteFile(external, []byte(`{"event":"EXTERNAL_EVENT_CANARY"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(s.RunDir("run-ev"), "events.ndjson")); err != nil {
		t.Fatal(err)
	}

	events, err := s.ReadEvents("run-ev")
	if err == nil {
		t.Fatalf("ReadEvents followed a symlinked events log and read %d external event(s)", len(events))
	}
}

// TestGateOutputIsConfined: the gate's output.log is the sharpest case — os.Create followed
// and truncated a planted symlink, streaming attacker-influenced gate output over it.
func TestGateOutputIsConfined(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-1", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}

	outside := filepath.Join(t.TempDir(), "victim")
	if err := os.WriteFile(outside, []byte("ORIGINAL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Pre-create the gate dir and plant output.log as a symlink to the victim.
	gateDir := s.GateDir("run-1", "verify", 1)
	if err := os.MkdirAll(gateDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(gateDir, "output.log")); err != nil {
		t.Fatal(err)
	}

	f, err := s.CreateGateOutput("run-1", "verify", 1)
	if err == nil {
		_, _ = f.WriteString("gate output the agent controls\n")
		_ = f.Close()
	}
	if data, _ := os.ReadFile(outside); string(data) != "ORIGINAL\n" {
		t.Fatalf("gate output was streamed through a planted symlink onto an external file: %q", data)
	}
}

// TestConfinementCatchesASymlinkedVichuDir: os.OpenRoot FOLLOWS a symlink at the root path,
// so rooting confinement at .vichu (when .vichu is itself a symlink an agent planted) would
// land writes in the symlink's external target. Rooting at the PROJECT catches it.
func TestConfinementCatchesASymlinkedVichuDir(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	project := t.TempDir()
	external := t.TempDir()
	// The agent replaces .vichu with a symlink to a directory outside the project.
	if err := os.Symlink(external, filepath.Join(project, ".vichu")); err != nil {
		t.Fatal(err)
	}
	s := Open(project)

	err := s.SaveState(&core.State{RunID: "run-1", Status: core.StatusActive})
	if err == nil {
		t.Fatal("writing through a symlinked .vichu must be refused")
	}
	if _, statErr := os.Stat(filepath.Join(external, "runs", "run-1", "state.json")); statErr == nil {
		t.Fatal("state was written into the external directory the .vichu symlink pointed at")
	}
}

// TestReserveGlobalOperationConfined: the op-id claim file lives under .vichu/, so reserving
// it through a symlinked .vichu must be refused, not create the record externally.
func TestReserveGlobalOperationConfined(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	project := t.TempDir()
	external := t.TempDir()
	if err := os.Symlink(external, filepath.Join(project, ".vichu")); err != nil {
		t.Fatal(err)
	}
	s := Open(project)

	var rec, existing struct{ X string }
	rec.X = "hi"
	_, err := s.ReserveGlobalOperation("run-start", "op1", &rec, &existing)
	if err == nil {
		t.Fatal("reserving through a symlinked .vichu must be refused")
	}
	if entries, _ := os.ReadDir(external); len(entries) != 0 {
		t.Fatalf("the reservation created %d entries in the external dir", len(entries))
	}
}

// TestReadConfigSnapshotRefusesInternalSymlink: an agent that replaces config.snapshot.yaml
// with a symlink — even one pointing back INSIDE the project at the live, tampered vichu.yaml
// — must be refused, not followed. os.Root.ReadFile follows an internal symlink; the frozen
// read must not.
func TestReadConfigSnapshotRefusesInternalSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	project := t.TempDir()
	if err := os.WriteFile(filepath.Join(project, "vichu.yaml"), []byte("TAMPERED\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := Open(project)
	if err := os.MkdirAll(s.RunDir("r1"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Legit snapshot first, then the agent swaps it for an internal symlink.
	if err := s.SaveConfigSnapshot("r1", []byte("FROZEN\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.ConfigSnapshotPath("r1")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../vichu.yaml", s.ConfigSnapshotPath("r1")); err != nil {
		t.Fatal(err)
	}

	data, err := s.ReadConfigSnapshot("r1")
	if err == nil {
		t.Fatalf("reading a symlinked snapshot must be refused, got content %q", data)
	}
	if string(data) == "TAMPERED\n" {
		t.Fatal("the read followed the internal symlink to the tampered live config")
	}
}

// TestContextPackDoesNotFollowSymlink: contextpack.md is injected into a worker's prompt, so a
// symlinked one must not be followed to an external file (that would copy a secret into the
// prompt). A symlink yields "" (no context), never the target's content.
func TestContextPackDoesNotFollowSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	project := t.TempDir()
	secret := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(secret, []byte("SSH KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s := Open(project)
	if err := os.MkdirAll(s.RunDir("r1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, s.ContextPackPath("r1")); err != nil {
		t.Fatal(err)
	}
	if got := s.ContextPack("r1"); got != "" {
		t.Fatalf("ContextPack followed a symlink and leaked external content: %q", got)
	}
}

// TestSaveStateRefusesFutureSchema: an older binary must not write a run whose schema it does
// not understand — that would drop the newer fields it cannot represent.
func TestSaveStateRefusesFutureSchema(t *testing.T) {
	s := Open(t.TempDir())
	err := s.SaveState(&core.State{RunID: "r1", Status: core.StatusActive, SchemaVersion: core.SchemaVersion + 1})
	if err == nil {
		t.Fatal("SaveState must refuse a future schema_version")
	}
}

// TestLoadArtifactRefusesSymlink (ronda 18): an artifact is evidence the kernel gates on, so a
// symlinked artifact pointing outside the store must be refused, not read as if it were ours.
func TestLoadArtifactRefusesSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-a", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external.md")
	if err := os.WriteFile(external, []byte("EXTERNAL_ARTIFACT_CANARY"), 0o644); err != nil {
		t.Fatal(err)
	}
	dir := s.ArtifactsDir("run-a")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dir, "plan.md")); err != nil {
		t.Fatal(err)
	}

	if _, err := s.LoadArtifact("run-a", "plan.md"); err == nil {
		t.Fatal("LoadArtifact followed a symlinked artifact to external data")
	}
}

// TestValidateEventLogCoherence (ronda 23): the audit validator rejects an empty log, a non-event
// `{}`, a log not starting with run_created, and a partial operation tuple (the seq=0 injection
// that suppresses a real worker_started via the dedup count).
func TestValidateEventLogCoherence(t *testing.T) {
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-1", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	ev := filepath.Join(s.RunDir("run-1"), "events.ndjson")
	good := `{"run":"run-1","event":"run_created","ts":"2026-01-01T00:00:00Z"}`
	bad := map[string]string{
		"empty":          "",
		"empty-object":   "{}\n",
		"no-run_created": `{"run":"run-1","event":"worker_started","ts":"2026-01-01T00:00:00Z"}` + "\n",
		"partial-tuple":  good + "\n" + `{"run":"run-1","event":"worker_started","ts":"2026-01-01T00:00:01Z","op_id":"x","op_fp":"y","seq":0}` + "\n",
		"different-run":  `{"run":"other","event":"run_created","ts":"2026-01-01T00:00:00Z"}` + "\n",
		// seq starts at 2 with no seq=1: the dedup count is 1, so the real seq=1 event is skipped as
		// "already written". The in-order per-op check rejects it (first event is not seq=1).
		"seq-gap": good + "\n" + `{"run":"run-1","event":"worker_completed","ts":"2026-01-01T00:00:02Z","op_id":"x","op_fp":"y","seq":2}` + "\n",
		// seq=1 appears twice under the same operation: a forged duplicate. Rejected (expected seq=2).
		"seq-dup": good + "\n" +
			`{"run":"run-1","event":"worker_started","ts":"2026-01-01T00:00:01Z","op_id":"x","op_fp":"y","seq":1}` + "\n" +
			`{"run":"run-1","event":"worker_started","ts":"2026-01-01T00:00:02Z","op_id":"x","op_fp":"y","seq":1}` + "\n",
		// seq=2 appears BEFORE seq=1 for the same op: the set {1,2} is complete, but the durable ORDER
		// is impossible (the append path only ever writes increasing seq). A set check would accept it;
		// the in-order check rejects it (first event is seq=2, expected 1).
		"seq-out-of-order": good + "\n" +
			`{"run":"run-1","event":"worker_completed","ts":"2026-01-01T00:00:02Z","op_id":"x","op_fp":"y","seq":2}` + "\n" +
			`{"run":"run-1","event":"worker_started","ts":"2026-01-01T00:00:01Z","op_id":"x","op_fp":"y","seq":1}` + "\n",
	}
	for name, content := range bad {
		if err := os.WriteFile(ev, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := s.ValidateEventLog("run-1"); err == nil {
			t.Fatalf("%s: ValidateEventLog must reject it", name)
		}
	}
	// A valid log passes — including two operations whose events INTERLEAVE (A1,B1,A2,B2): each op
	// advances its own counter, so interleaving is legal as long as each op's seqs appear in order.
	ok := good + "\n" +
		`{"run":"run-1","event":"worker_started","ts":"2026-01-01T00:00:01Z","op_id":"a","op_fp":"fa","seq":1}` + "\n" +
		`{"run":"run-1","event":"worker_started","ts":"2026-01-01T00:00:02Z","op_id":"b","op_fp":"fb","seq":1}` + "\n" +
		`{"run":"run-1","event":"worker_completed","ts":"2026-01-01T00:00:03Z","op_id":"a","op_fp":"fa","seq":2}` + "\n" +
		`{"run":"run-1","event":"worker_completed","ts":"2026-01-01T00:00:04Z","op_id":"b","op_fp":"fb","seq":2}` + "\n"
	if err := os.WriteFile(ev, []byte(ok), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := s.ValidateEventLog("run-1"); err != nil {
		t.Fatalf("a coherent (interleaved) log must pass: %v", err)
	}
}

// TestLoadVerifiedEvents (ronda 24): the read-once API used by status and cancel must validate AND
// return the SAME snapshot — a coherent log yields its events, a corrupt one yields an error (never a
// partial slice a caller could act on), and a missing one is an error (a materialized run always has
// run_created). This closes the TOCTOU of ValidateEventLog followed by a second ReadEvents.
func TestLoadVerifiedEvents(t *testing.T) {
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-1", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	ev := filepath.Join(s.RunDir("run-1"), "events.ndjson")
	good := `{"run":"run-1","event":"run_created","ts":"2026-01-01T00:00:00Z"}`

	// Coherent → the parsed events come back.
	full := good + "\n" + `{"run":"run-1","event":"worker_started","ts":"2026-01-01T00:00:01Z","op_id":"x","op_fp":"y","seq":1}` + "\n"
	if err := os.WriteFile(ev, []byte(full), 0o644); err != nil {
		t.Fatal(err)
	}
	events, err := s.LoadVerifiedEvents("run-1")
	if err != nil {
		t.Fatalf("a coherent log must load: %v", err)
	}
	if len(events) != 2 || events[0].Event != core.EventRunCreated {
		t.Fatalf("expected 2 events beginning with run_created, got %+v", events)
	}

	// Incoherent-but-parseable (no run_created first) → an error and NO events. This is exactly what
	// ReadEvents would TOLERATE (returning the slice); LoadVerifiedEvents must not, or status/cancel
	// would act on a slice they never proved coherent.
	incoherent := `{"run":"run-1","event":"worker_started","ts":"2026-01-01T00:00:01Z"}` + "\n"
	if err := os.WriteFile(ev, []byte(incoherent), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, err := s.LoadVerifiedEvents("run-1"); err == nil || got != nil {
		t.Fatalf("an incoherent log must error with no events, got events=%+v err=%v", got, err)
	}

	// Missing → an error (a materialized run always carries run_created).
	if err := os.Remove(ev); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadVerifiedEvents("run-1"); err == nil {
		t.Fatal("a missing log must error")
	}
}

// TestOpenVerifiedAuditHappyPath (ronda 26): OpenVerifiedAudit validates and returns the events, and
// Append writes through the held descriptor to the SAME file, which the run's log then reflects.
func TestOpenVerifiedAuditHappyPath(t *testing.T) {
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-1", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	ev := filepath.Join(s.RunDir("run-1"), "events.ndjson")
	if err := os.WriteFile(ev, []byte(`{"run":"run-1","event":"run_created","ts":"2026-01-01T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a, events, err := s.OpenVerifiedAudit("run-1")
	if err != nil {
		t.Fatalf("OpenVerifiedAudit on a coherent log: %v", err)
	}
	defer a.Close()
	if len(events) != 1 || events[0].Event != core.EventRunCreated {
		t.Fatalf("expected [run_created], got %+v", events)
	}
	if err := a.Append(core.Event{Run: "run-1", Event: core.EventRunCanceled}); err != nil {
		t.Fatalf("Append through the held descriptor: %v", err)
	}
	got, _ := os.ReadFile(ev)
	if !strings.Contains(string(got), "run_canceled") {
		t.Fatalf("the log must reflect the appended event, got:\n%s", got)
	}
}

// TestOpenVerifiedAuditDetectsReplacement (ronda 26) is the ATOMIC-REPLACEMENT regression: a log
// REPLACED (not just deleted) between validation and append must be caught. The reviewer reproduced
// cancel exiting 0 while 600k events collapsed to run_created + run_canceled — the append had landed
// on the replacement. With one held descriptor the append goes to the ORIGINAL inode, and the identity
// check reports the repointed path instead of lying.
func TestOpenVerifiedAuditDetectsReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("rename-over-open-file semantics differ on Windows")
	}
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-1", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	ev := filepath.Join(s.RunDir("run-1"), "events.ndjson")
	original := `{"run":"run-1","event":"run_created","ts":"2026-01-01T00:00:00Z"}` + "\n" +
		`{"run":"run-1","event":"worker_started","ts":"2026-01-01T00:00:01Z","op_id":"x","op_fp":"y","seq":1}` + "\n"
	if err := os.WriteFile(ev, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	a, _, err := s.OpenVerifiedAudit("run-1")
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close()

	// Atomically REPLACE the path with a DIFFERENT file (new inode) while the descriptor is held.
	replacement := filepath.Join(s.RunDir("run-1"), "events.ndjson.new")
	if err := os.WriteFile(replacement, []byte(`{"run":"run-1","event":"run_created","ts":"2026-01-01T00:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, ev); err != nil {
		t.Fatal(err)
	}

	// Append must FAIL (the path no longer resolves to the validated inode).
	if err := a.Append(core.Event{Run: "run-1", Event: core.EventRunCanceled}); err == nil {
		t.Fatal("Append accepted a run_canceled onto a log that had been replaced")
	}
	// The REPLACEMENT (now at the path) must NOT have absorbed the event — it went to the original.
	got, _ := os.ReadFile(ev)
	if strings.Contains(string(got), "run_canceled") {
		t.Fatalf("the replacement file absorbed the event; history was lost:\n%s", got)
	}
}

// TestValidateRunID (ronda 27): a run id is a single path component. Reject traversal, separators,
// absolute paths, control chars, empty and over-long; accept minted, test and hand-created ids.
func TestValidateRunID(t *testing.T) {
	bad := []string{
		"", ".", "..", "../victim", "../../victim", "a/b", `a\b`, "/abs", "run\x00id", "run\nid",
		strings.Repeat("x", 129),
	}
	for _, id := range bad {
		if err := ValidateRunID(id); err == nil {
			t.Errorf("ValidateRunID(%q) = nil, want error", id)
		}
	}
	good := []string{
		"run-20260714-050110-e69727fd9637", // minted
		"run-1",                            // test id
		"legacy_run.42",                    // hand-created, safe component
		"hostpack",                         // the reserved lock scope
	}
	for _, id := range good {
		if err := ValidateRunID(id); err != nil {
			t.Errorf("ValidateRunID(%q) = %v, want nil", id, err)
		}
	}
}

// TestStoreRefusesTraversalRunID (ronda 27) is the path-traversal regression: a run id like
// "../../victim" resolves (via filepath.Join onto .vichu/runs) to <project>/victim — inside the
// project, so confinement allows it, but OUTSIDE the runs directory. Every Store entry must refuse it
// before building a path, so a run operation cannot read or write another location in the project.
func TestStoreRefusesTraversalRunID(t *testing.T) {
	project := t.TempDir()
	s := Open(project)
	// A would-be victim OUTSIDE .vichu/runs but inside the project.
	victimDir := filepath.Join(project, "victim")
	victimState := filepath.Join(victimDir, "state.json")
	if err := os.MkdirAll(victimDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(victimState, []byte(`{"run_id":"victim","status":"active"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	trav := "../../victim" // from .vichu/runs: runs/.. = .vichu, .vichu/.. = project, then /victim
	if s.RunExists(trav) {
		t.Fatal("RunExists accepted a traversal run id")
	}
	if _, err := s.LoadState(trav); err == nil {
		t.Fatal("LoadState accepted a traversal run id")
	}
	if err := s.SaveState(&core.State{RunID: trav, Status: core.StatusCanceled}); err == nil {
		t.Fatal("SaveState accepted a traversal run id")
	}
	if err := s.AppendEvent(core.Event{Run: trav, Event: core.EventRunCanceled}); err == nil {
		t.Fatal("AppendEvent accepted a traversal run id")
	}
	if _, _, err := s.OpenVerifiedAudit(trav); err == nil {
		t.Fatal("OpenVerifiedAudit accepted a traversal run id")
	}
	if _, err := s.AcquireLockExisting(trav); err == nil {
		t.Fatal("AcquireLockExisting accepted a traversal run id")
	}

	// The victim is untouched: no run_canceled, no events.ndjson, no lock created next to it.
	got, _ := os.ReadFile(victimState)
	if strings.Contains(string(got), "canceled") {
		t.Fatalf("a traversal run id mutated state.json outside .vichu/runs:\n%s", got)
	}
	if _, err := os.Stat(filepath.Join(victimDir, "events.ndjson")); err == nil {
		t.Fatal("a traversal run id created events.ndjson outside .vichu/runs")
	}
	if _, err := os.Stat(filepath.Join(victimDir, "lock.json")); err == nil {
		t.Fatal("a traversal run id created a lock outside .vichu/runs")
	}
}

// TestLoadStateRejectsMismatchedRunID (ronda 27): the state read from runs/<id> must identify as
// <id>. A state.json whose run_id names a DIFFERENT run means the id resolved to the wrong run's
// state — do not act on it.
func TestLoadStateRejectsMismatchedRunID(t *testing.T) {
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-2", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	// Move runs/run-2 to runs/run-1, so runs/run-1/state.json still says run_id "run-2".
	if err := os.Rename(s.RunDir("run-2"), s.RunDir("run-1")); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadState("run-1"); err == nil {
		t.Fatal("LoadState accepted a state.json whose run_id is run-2 under runs/run-1")
	}
}

// TestOpenVerifiedAuditRejectsMissingAndCorrupt (ronda 26): a missing or corrupt log yields a nil
// handle and an error, so cancel's escape hatch reports the loss instead of appending onto nothing.
func TestOpenVerifiedAuditRejectsMissingAndCorrupt(t *testing.T) {
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-1", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	ev := filepath.Join(s.RunDir("run-1"), "events.ndjson")
	// Missing.
	if a, _, err := s.OpenVerifiedAudit("run-1"); err == nil || a != nil {
		t.Fatalf("missing log must yield nil handle + error, got a=%v err=%v", a, err)
	}
	// Corrupt (parseable but not starting with run_created).
	if err := os.WriteFile(ev, []byte(`{"run":"run-1","event":"worker_started","ts":"2026-01-01T00:00:01Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if a, _, err := s.OpenVerifiedAudit("run-1"); err == nil || a != nil {
		t.Fatalf("corrupt log must yield nil handle + error, got a=%v err=%v", a, err)
	}
}

// TestAcquireLockExistingDoesNotCreate (ronda 25): acquiring the lock for a run that does not exist
// must return ErrRunNotFound and NOT materialize its directory — a rejected resume/op leaves no
// trace (I2). Compare AcquireLock, which creates on purpose (run birth).
func TestAcquireLockExistingDoesNotCreate(t *testing.T) {
	project := t.TempDir()
	s := Open(project)
	if _, err := s.AcquireLockExisting("ghost"); !errors.Is(err, ErrRunNotFound) {
		t.Fatalf("AcquireLockExisting on a nonexistent run = %v, want ErrRunNotFound", err)
	}
	if _, err := os.Stat(s.RunDir("ghost")); err == nil {
		t.Fatal("AcquireLockExisting materialized the directory of a nonexistent run")
	}
}

// TestLoadReviewVerdictValidatesPersisted (ronda 23): a PERSISTED verdict is evidence the kernel
// branches on — a legacy/forged approved+blocker on disk must not approve a review.
func TestLoadReviewVerdictValidatesPersisted(t *testing.T) {
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-1", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	dir := s.ReviewDir("run-1", "review", 1)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// approved WITH a blocker finding — contradictory.
	bad := `{"status":"approved","findings":[{"severity":"blocker","message":"must fix"}]}`
	if err := os.WriteFile(filepath.Join(dir, "verdict.json"), []byte(bad), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadReviewVerdict("run-1", "review", 1); err == nil {
		t.Fatal("LoadReviewVerdict accepted a persisted approved+blocker verdict")
	}
}

// TestLoadReviewVerdictRejectsUnknownStatus (ronda 24): a persisted verdict whose STATUS is not a
// known value must not load. Otherwise decideFromVerdict's default arm presents the unknown as a
// "reviewer blocked the run" the reviewer never issued — the kernel inventing a verdict from a
// corrupt/forged file. Validation of the status must live in the persisted-evidence path, not only
// at ingress.
func TestLoadReviewVerdictRejectsUnknownStatus(t *testing.T) {
	project := t.TempDir()
	s := Open(project)
	if err := s.CreateRun(&core.State{RunID: "run-1", Status: core.StatusActive}); err != nil {
		t.Fatal(err)
	}
	dir := s.ReviewDir("run-1", "review", 1)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "verdict.json"), []byte(`{"status":"maybe"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.LoadReviewVerdict("run-1", "review", 1); err == nil {
		t.Fatal("LoadReviewVerdict accepted a persisted verdict with an unknown status")
	}
}
