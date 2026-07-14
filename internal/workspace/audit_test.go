package workspace

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/corteshvictor/vichu-flow/internal/core"
)

// mutFor returns the mutation for path, or fails the test naming what WAS reported —
// "not found" is the failure mode of every audit blind spot below, so say what we saw.
func mutFor(t *testing.T, muts []core.Mutation, path string) core.Mutation {
	t.Helper()
	for _, m := range muts {
		if m.Path == path {
			return m
		}
	}
	got := make([]string, 0, len(muts))
	for _, m := range muts {
		got = append(got, m.Path)
	}
	t.Fatalf("no mutation reported for %q; the audit saw: %v", path, got)
	return core.Mutation{}
}

// TestRollbackNeverWritesThroughASymlink is the load-bearing test for the rollback:
// backup and restore must never resolve the final path element.
//
// The bug: backup used os.ReadFile and restore used os.WriteFile, both of which FOLLOW
// symlinks. So if the thing sitting at a backed-up path was a symlink by the time the
// rollback ran, the restore wrote the backed-up bytes into whatever it pointed at — and
// VichuFlow, having just blocked the gate for changing the tree, wrote OUTSIDE the project
// itself. That is the one thing it promises never to do.
//
// Two shapes, both exercised here, because they fail differently:
//
//	A. a REGULAR file the gate replaces with a link — the restore must not write through it
//	B. a SYMLINK the gate retargets — the restore must put the link text back, not the
//	   target's content
func TestRollbackNeverWritesThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	dir := initRepo(t)

	// A file outside the project that nothing in the workspace may touch.
	outside := filepath.Join(t.TempDir(), "victim.txt")
	if err := os.WriteFile(outside, []byte("ORIGINAL\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A versioned symlink — the shape you get from a `config -> config.dev.yaml` checkout.
	writeFile(t, dir, "notes.txt", "committed\n")
	writeFile(t, dir, "config.dev.yaml", "env: dev\n")
	writeFile(t, dir, "config.prod.yaml", "env: prod\n")
	if err := os.Symlink("config.dev.yaml", filepath.Join(dir, "config")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "add config symlink")

	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}

	// The worker changes both, so BOTH are in the changed-vs-baseline set the backup
	// captures. That is the precondition for the rollback to ever touch them.
	writeFile(t, dir, "notes.txt", "the worker's edit\n")
	retarget(t, dir, "config", "config.prod.yaml")

	backup, err := repo.BackupChanged()
	if err != nil {
		t.Fatalf("BackupChanged: %v", err)
	}

	// Now the gate misbehaves: it points both paths at the file outside the project.
	retarget(t, dir, "notes.txt", outside)
	retarget(t, dir, "config", outside)

	if _, err := backup.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	// The external file is untouched. This is the assertion the old code failed.
	assertRegular(t, outside, "ORIGINAL\n")
	// (A) the regular file came back as a regular file, with the worker's content.
	assertRegular(t, filepath.Join(dir, "notes.txt"), "the worker's edit\n")
	// (B) the symlink came back as a symlink, pointing where the backup found it.
	assertSymlink(t, filepath.Join(dir, "config"), "config.prod.yaml")
}

// retarget replaces whatever is at dir/name with a symlink to target, the way a
// misbehaving gate would.
func retarget(t *testing.T, dir, name, target string) {
	t.Helper()
	full := filepath.Join(dir, name)
	if err := os.Remove(full); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, full); err != nil {
		t.Fatal(err)
	}
}

// assertRegular fails unless full is a regular file — not a symlink to one — holding want.
func assertRegular(t *testing.T, full, want string) {
	t.Helper()
	fi, err := os.Lstat(full)
	if err != nil {
		t.Fatalf("%s: %v", full, err)
	}
	if !fi.Mode().IsRegular() {
		t.Fatalf("%s: want a regular file, got mode %v", full, fi.Mode())
	}
	data, err := os.ReadFile(full)
	if err != nil {
		t.Fatalf("%s: %v", full, err)
	}
	if string(data) != want {
		t.Fatalf("%s: content is %q, want %q", full, data, want)
	}
}

// assertSymlink fails unless full is a symlink whose target text is want.
func assertSymlink(t *testing.T, full, want string) {
	t.Helper()
	target, err := os.Readlink(full)
	if err != nil {
		t.Fatalf("%s: want a symlink: %v", full, err)
	}
	if target != want {
		t.Fatalf("%s: points at %q, want %q", full, target, want)
	}
}

// TestRetargetedSymlinkIsAMutation: a symlink's identity is its TARGET TEXT. Hashing
// through it fingerprinted the content it pointed at, so retargeting a link between two
// files with identical content changed nothing the audit could see.
func TestRetargetedSymlinkIsAMutation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	dir := initRepo(t)
	writeFile(t, dir, "a.txt", "same\n")
	writeFile(t, dir, "b.txt", "same\n") // identical content — the whole point
	if err := os.Symlink("a.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "add link")

	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	tracker, err := repo.BeginTracking()
	if err != nil {
		t.Fatalf("BeginTracking: %v", err)
	}

	if err := os.Remove(filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("b.txt", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}

	muts, err := tracker.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	m := mutFor(t, muts, "link")
	if !strings.HasPrefix(m.Hash, symlinkHashPrefix) {
		t.Fatalf("a symlink must be fingerprinted by its target text, got hash %q", m.Hash)
	}
}

// TestOverwrittenUntrackedFileIsModified: git's status codes are relative to HEAD, not to
// the worker. A file that was ALREADY untracked before the worker still reads "??" after
// the worker overwrites it — so it was classified `untracked`, and the gate policy only
// blocks `modified`/`deleted`. A gate could destroy pre-existing user work and the run
// still reached `completed`.
func TestOverwrittenUntrackedFileIsModified(t *testing.T) {
	dir := initRepo(t)
	writeFile(t, dir, "notes.txt", "my unfinished work\n") // untracked, never committed

	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	tracker, err := repo.BeginTracking()
	if err != nil {
		t.Fatalf("BeginTracking: %v", err)
	}

	writeFile(t, dir, "notes.txt", "clobbered\n") // a gate overwrites it
	writeFile(t, dir, "fresh.txt", "created\n")   // and creates a genuinely new one

	muts, err := tracker.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	if k := mutFor(t, muts, "notes.txt").Kind; k != core.MutationModified {
		t.Fatalf("overwriting pre-existing untracked work is a MODIFICATION, got %q", k)
	}
	if k := mutFor(t, muts, "fresh.txt").Kind; k != core.MutationUntracked {
		t.Fatalf("a file the worker actually created is untracked, got %q", k)
	}
}

// TestIgnoredFileIsAudited: `git status` omits ignored paths, so a worker that overwrote a
// gitignored file produced "mutations: null" — the audit could not see what git does not
// report. The path is now RECORDED (with its hash) and marked Derived. Derived is informational:
// it lets the GATE path allow a build's new artifact (coverage), but it does NOT exempt a worker
// mutation (only HostBookkeeping is exempt there) — so this overwrite is still audited, not hidden.
func TestIgnoredFileIsAudited(t *testing.T) {
	dir := initRepo(t)
	writeFile(t, dir, ".gitignore", "coverage.out\n.env\n")
	writeFile(t, dir, "coverage.out", "old\n")
	writeFile(t, dir, ".env", "SECRET=old\n")
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "ignore")

	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	tracker, err := repo.BeginTracking()
	if err != nil {
		t.Fatalf("BeginTracking: %v", err)
	}

	writeFile(t, dir, "coverage.out", "new\n")
	writeFile(t, dir, ".env", "SECRET=stolen\n")

	muts, err := tracker.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}

	cov := mutFor(t, muts, "coverage.out")
	if !cov.Derived || cov.Sensitive {
		t.Fatalf("coverage.out: want derived and not sensitive, got derived=%v sensitive=%v", cov.Derived, cov.Sensitive)
	}
	if cov.Kind != core.MutationModified || cov.Hash == "" {
		t.Fatalf("coverage.out: want a modification with a real hash, got kind=%q hash=%q", cov.Kind, cov.Hash)
	}
	// .env is derived too (it is gitignored) — but sensitive, which is what keeps it
	// POLICED. A gate rewriting a coverage file is normal; one rewriting .env is not.
	env := mutFor(t, muts, ".env")
	if !env.Derived || !env.Sensitive {
		t.Fatalf(".env: want derived AND sensitive, got derived=%v sensitive=%v", env.Derived, env.Sensitive)
	}
}

// TestIgnoredDirectoryIsNotEnumerated is the other half of the deal above, and it is a
// deliberate line, not an oversight: a project that ignores a whole DIRECTORY has declared
// that subtree derived output. Walking and hashing 50k node_modules files on every worker
// start and finish would cost far more than it buys, so the directory collapses to one
// entry that we skip. It is a documented limit.
func TestIgnoredDirectoryIsNotEnumerated(t *testing.T) {
	dir := initRepo(t)
	writeFile(t, dir, ".gitignore", "node_modules/\n")
	writeFile(t, dir, "node_modules/pkg/index.js", "x\n")
	runGit(t, dir, "add", ".gitignore")
	runGit(t, dir, "commit", "-m", "ignore")

	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	changed, err := repo.captureChanged()
	if err != nil {
		t.Fatalf("captureChanged: %v", err)
	}
	for p := range changed {
		if strings.HasPrefix(p, "node_modules") {
			t.Fatalf("an ignored DIRECTORY must not be enumerated, got %q", p)
		}
	}
}

// TestPorcelainPathsSurviveQuotingAndSpaces: git C-escapes any path outside plain ASCII
// (core.quotepath is on by default), so "café.txt" arrived as the literal "caf\303\251.txt"
// — a path that does not exist on disk. It therefore hashed to "", and a gate could
// overwrite the real file with the diff seeing nothing change.
func TestPorcelainPathsSurviveQuotingAndSpaces(t *testing.T) {
	if runtime.GOOS == "windows" {
		// One of the awkward names below contains '>' and ' -> ', which are invalid in a Windows
		// filename — the case being exercised (git C-quotepath escaping of non-ASCII/awkward paths)
		// is the same on every platform, so covering it on Unix is enough.
		t.Skip("awkward filenames with '>' cannot exist on Windows")
	}
	dir := initRepo(t)
	names := []string{"café.txt", "my notes.txt", "weird -> name.txt"}
	for _, n := range names {
		writeFile(t, dir, n, "before\n")
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "add awkward names")

	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	tracker, err := repo.BeginTracking()
	if err != nil {
		t.Fatalf("BeginTracking: %v", err)
	}
	for _, n := range names {
		writeFile(t, dir, n, "after\n")
	}

	muts, err := tracker.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	for _, n := range names {
		m := mutFor(t, muts, n)
		if m.Kind != core.MutationModified {
			t.Fatalf("%q: want modified, got %q", n, m.Kind)
		}
		if m.Hash == "" {
			t.Fatalf("%q: empty hash — the audit is looking at a path that does not exist", n)
		}
	}
}

// TestParsePorcelainZ covers the record framing directly: a rename emits a SECOND record
// holding the original path, which must be consumed rather than read as a status of its own.
func TestParsePorcelainZ(t *testing.T) {
	out := "R  new name.txt\x00old name.txt\x00 M src/main.go\x00?? untracked.txt\x00!! coverage.out\x00"
	recs := parsePorcelainZ(out)
	want := []porcelainRec{
		{code: "R ", path: "new name.txt"},
		{code: " M", path: "src/main.go"},
		{code: "??", path: "untracked.txt"},
		{code: "!!", path: "coverage.out"},
	}
	if len(recs) != len(want) {
		t.Fatalf("got %d records %+v, want %d", len(recs), recs, len(want))
	}
	for i, w := range want {
		if recs[i] != w {
			t.Errorf("record %d: got %+v, want %+v", i, recs[i], w)
		}
	}
}

// TestUnreadableFileIsNotTreatedAsMissing is the second load-bearing test of this layer.
//
// The bug: every error from Lstat/ReadFile collapsed into "the file is not there". So a
// file that EXISTS but cannot be READ — mode 000, a denied ACL — hashed to "", which is
// exactly what an absent path hashes to. It therefore looked like a path the worker had
// just created: it was never backed up, and a gate could chmod it, overwrite it, and leave
// the run reaching `completed` with the original content gone.
//
// Not being able to read a file is not evidence that it is absent. It is a reason to stop.
func TestUnreadableFileIsNotTreatedAsMissing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes; the Windows equivalent is a denied ACL")
	}
	if os.Geteuid() == 0 {
		t.Skip("root reads everything")
	}
	dir := initRepo(t)
	secret := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secret, []byte("PRIVATE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(secret, 0o000); err != nil { // exists, and we cannot read it
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(secret, 0o600) })

	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	// Tracking must refuse: we cannot audit what we cannot read.
	if _, err := repo.BeginTracking(); err == nil {
		t.Fatal("BeginTracking must fail on a file it cannot read — treating it as absent is how it got overwritten")
	}
	// And the backup must refuse, which is what stops the gate from running at all: with no
	// copy of the original, a rollback could not undo whatever the gate does to it.
	if _, err := repo.BackupChanged(); err == nil {
		t.Fatal("BackupChanged must fail on a file it cannot read — a gate must not run without a backup")
	}
}

// TestLegacyBeforeSnapshotFailsClosed: a snapshot written before `exists` was recorded
// cannot say whether an empty-hash path was ABSENT or merely UNREADABLE. Those need
// opposite handling, so the worker is restarted rather than guessed at.
func TestLegacyBeforeSnapshotFailsClosed(t *testing.T) {
	dir := initRepo(t)
	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	// Exactly what an older binary persisted: code + hash, no `exists`.
	legacy := map[string]core.FileSig{"secret.txt": {Code: "??", Hash: ""}}
	if _, err := repo.ResumeTracking(legacy); err == nil {
		t.Fatal("an ambiguous legacy snapshot must fail closed, not be guessed at")
	}

	// The unambiguous legacy cases still resume: a real hash proves the file was read.
	ok := map[string]core.FileSig{"a.txt": {Code: " M", Hash: "abc123"}, "gone.txt": {Code: " D", Hash: ""}}
	if _, err := repo.ResumeTracking(ok); err != nil {
		t.Fatalf("an unambiguous legacy snapshot must still resume: %v", err)
	}
}

// TestGateCannotBlockTheRollbackByPredictingTheTemp: the temp file a restore writes through
// used to be named from a hash OF THE PATH — so a gate could compute it, pre-create it, and
// make the O_EXCL open fail. The rollback that would have undone the gate's damage failed
// instead, and the user's file stayed clobbered.
//
// The name is now 128 bits from crypto/rand. There is nothing to predict.
func TestGateCannotBlockTheRollbackByPredictingTheTemp(t *testing.T) {
	dir := initRepo(t)
	writeFile(t, dir, "draft.txt", "USER\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "add draft")

	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	writeFile(t, dir, "draft.txt", "the worker's edit\n")
	backup, err := repo.BackupChanged()
	if err != nil {
		t.Fatalf("BackupChanged: %v", err)
	}

	// The gate clobbers the file AND litters the directory with every name a
	// path-derived scheme could have produced, plus a pile of decoys.
	writeFile(t, dir, "draft.txt", "GATE\n")
	sum := sha256.Sum256([]byte("draft.txt"))
	for _, squat := range []string{
		".vichu-restore-" + hex.EncodeToString(sum[:8]),
		".vichu-tmp-" + hex.EncodeToString(sum[:8]),
		".vichu-tmp-draft.txt",
		".vichu-restore-draft.txt",
	} {
		writeFile(t, dir, squat, "squatted\n")
	}

	if _, err := backup.Restore(); err != nil {
		t.Fatalf("Restore: %v — a gate must not be able to make the rollback fail", err)
	}
	assertRegular(t, filepath.Join(dir, "draft.txt"), "the worker's edit\n")
}

// TestRollbackOverANonEmptyDirectory: a gate replaced the file with a DIRECTORY. Remove()
// cannot delete a non-empty one, so the rollback failed and the user's file stayed gone —
// while the run reported that it had blocked and restored.
//
// The directory is moved to .vichu/rollback/ rather than deleted: we did not create it and
// we have not looked inside it, so destroying it to undo a destruction is not a trade we get
// to make. The original comes back; the directory is preserved as evidence.
func TestRollbackOverANonEmptyDirectory(t *testing.T) {
	dir := initRepo(t)
	writeFile(t, dir, "draft.txt", "USER\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "add draft")

	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	writeFile(t, dir, "draft.txt", "the worker's edit\n")
	backup, err := repo.BackupChanged()
	if err != nil {
		t.Fatalf("BackupChanged: %v", err)
	}

	// The gate swaps the file for a non-empty directory.
	if err := os.Remove(filepath.Join(dir, "draft.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "draft.txt/generated.js", "build output\n")

	if _, err := backup.Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}
	assertRegular(t, filepath.Join(dir, "draft.txt"), "the worker's edit\n")

	// The directory's contents survived, in quarantine, with a record saying where they came
	// from. Nothing the gate produced was destroyed to make room.
	found := false
	_ = filepath.WalkDir(filepath.Join(dir, ".vichu", "rollback"), func(p string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() && filepath.Base(p) == "generated.js" {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatal("the gate's directory must be quarantined under .vichu/rollback/, not deleted")
	}
}

// TestGateReplacingAFileWithADirIsDetected is the DETECTION half of the file→directory case
// (the rollback half is TestRollbackOverANonEmptyDirectory). A regular file that existed when
// the worker started, replaced by a directory, must be reported as a mutation — not dropped
// because "the path still exists" (it does, as a directory). Covered for both providers.
func TestGateReplacingAFileWithADirIsDetected(t *testing.T) {
	t.Run("git", func(t *testing.T) {
		dir := initRepo(t)
		writeFile(t, dir, "victim.txt", "USER WORK\n") // untracked, created "by the worker"
		repo, err := Detect(dir)
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		assertFileToDirDetected(t, repo, dir)
	})
	t.Run("filesystem", func(t *testing.T) {
		w, dir := fsWorkspace(t)
		if _, err := w.Snapshot(""); err != nil {
			t.Fatal(err)
		}
		writeFile(t, dir, "victim.txt", "USER WORK\n")
		assertFileToDirDetected(t, w, dir)
	})
}

// assertFileToDirDetected tracks from the point victim.txt exists, replaces it with a
// non-empty directory, and asserts the tracker reports victim.txt as a destructive change
// (not as merely the new file inside the directory).
func assertFileToDirDetected(t *testing.T, src interface {
	BeginTracking() (*Tracker, error)
}, dir string,
) {
	t.Helper()
	tracker, err := src.BeginTracking()
	if err != nil {
		t.Fatalf("BeginTracking: %v", err)
	}
	// The gate replaces the regular file with a non-empty directory.
	if err := os.Remove(filepath.Join(dir, "victim.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "victim.txt/generated.txt", "build output\n")

	muts, err := tracker.Finish()
	if err != nil {
		t.Fatalf("Finish: %v", err)
	}
	m := mutFor(t, muts, "victim.txt")
	if m.Kind != core.MutationModified && m.Kind != core.MutationDeleted {
		t.Fatalf("victim.txt replaced by a directory must be modified/deleted, got %q", m.Kind)
	}
}

// TestQuarantineLeavesNoFalseRecordWhenRenameFails: when the move that quarantines a directory
// fails (e.g. a read-only parent), the rollback must NOT leave a record claiming the data was
// displaced, and must not say "it is quarantined at". Nothing moved, so it says nothing moved.
func TestQuarantineLeavesNoFalseRecordWhenRenameFails(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses chmod on the parent dir")
	}
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory write bits")
	}
	dir := initRepo(t)
	writeFile(t, dir, "sub/victim.txt", "USER\n")
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-m", "add sub/victim.txt")

	repo, err := Detect(dir)
	if err != nil {
		t.Fatalf("Detect: %v", err)
	}
	writeFile(t, dir, "sub/victim.txt", "worker edit\n")
	backup, err := repo.BackupChanged()
	if err != nil {
		t.Fatalf("BackupChanged: %v", err)
	}

	// The gate turns the file into a directory and makes the PARENT read-only, so the
	// quarantine rename cannot succeed.
	if err := os.Remove(filepath.Join(dir, "sub", "victim.txt")); err != nil {
		t.Fatal(err)
	}
	writeFile(t, dir, "sub/victim.txt/generated.txt", "gate output\n")
	if err := os.Chmod(filepath.Join(dir, "sub"), 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(dir, "sub"), 0o755) })

	_, rerr := backup.Restore()
	if rerr == nil {
		t.Fatal("restore through a read-only parent must report failure, not silent success")
	}
	if strings.Contains(rerr.Error(), "it is quarantined at") {
		t.Fatalf("the error claims the data was quarantined when the move failed: %v", rerr)
	}

	// No record file must be left claiming a displacement that never happened.
	_ = os.Chmod(filepath.Join(dir, "sub"), 0o755)
	rollback := filepath.Join(dir, ".vichu", "rollback")
	entries, _ := os.ReadDir(rollback)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".json") {
			t.Fatalf("a quarantine record %q was left asserting a move that failed", e.Name())
		}
	}
}

// TestFilesystemRebaselineRefusesSymlinkedVichu: rebaseline does os.RemoveAll under .vichu,
// which follows a symlinked parent. A .vichu symlink pointing outside the project would make
// it delete external data — so it is refused before any removal.
func TestFilesystemRebaselineRefusesSymlinkedVichu(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need elevation on Windows")
	}
	project := t.TempDir()
	external := t.TempDir()
	if err := os.MkdirAll(filepath.Join(external, "baseline"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(external, "baseline", "keep.txt"), []byte("KEEP\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(project, ".vichu")); err != nil {
		t.Fatal(err)
	}

	w, err := OpenFilesystem(project)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Snapshot(""); err == nil {
		t.Fatal("Snapshot must refuse a symlinked .vichu, not RemoveAll through it")
	}
	if _, err := os.Stat(filepath.Join(external, "baseline", "keep.txt")); err != nil {
		t.Fatal("external baseline data was deleted through the .vichu symlink")
	}
}
