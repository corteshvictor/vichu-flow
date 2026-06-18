package core

import "time"

// ArtifactCatalog is the allowlist of named host-first artifacts and the fixed
// filenames the kernel writes them to under a run's artifacts/ directory. The
// `--artifact <name>=<file>` flag accepts only these logical names — never a path
// — so a host can never write outside the runtime or overwrite arbitrary files.
var ArtifactCatalog = map[string]string{
	"proposal":    "proposal.md",
	"plan":        "plan.md",
	"test_intent": "test_intent.md",
}

// ArtifactFilename returns the fixed runtime filename for a logical artifact name,
// or ("", false) if the name is not in the allowlist.
func ArtifactFilename(name string) (string, bool) {
	f, ok := ArtifactCatalog[name]
	return f, ok
}

// DefaultArtifactForStage maps a stage to the artifact its result becomes when
// the host does not pass an explicit --artifact (e.g. propose → proposal). Empty
// means the stage has no default artifact.
func DefaultArtifactForStage(stage string) string {
	switch stage {
	case "propose":
		return "proposal"
	case "plan":
		return "plan"
	default:
		return ""
	}
}

// ArtifactsAllowedForStage returns the artifacts a stage is permitted to produce. A
// stage may only write artifacts it OWNS — propose owns `proposal`, plan owns `plan`
// (and the optional `test_intent`); other stages own none. This keeps each stage's
// evidence its own (the SDD guarantee) instead of inheriting a file an earlier stage
// wrote.
func ArtifactsAllowedForStage(stage string) []string {
	switch stage {
	case "propose":
		return []string{"proposal"}
	case "plan":
		return []string{"plan", "test_intent"}
	default:
		return nil
	}
}

// ArtifactAllowedForStage reports whether stage may produce the named artifact.
func ArtifactAllowedForStage(stage, name string) bool {
	for _, a := range ArtifactsAllowedForStage(stage) {
		if a == name {
			return true
		}
	}
	return false
}

// ArtifactMeta records which stage entry produced an artifact, so the kernel can
// verify a required artifact is evidence from THIS stage and iteration — not a stale
// file left by another stage or an earlier iteration.
type ArtifactMeta struct {
	Name       string    `json:"name"`
	Filename   string    `json:"filename"`
	Stage      string    `json:"stage"`
	WorkerID   string    `json:"worker_id,omitempty"`
	Iteration  int       `json:"iteration"`
	SHA256     string    `json:"sha256"`
	CapturedAt time.Time `json:"captured_at"`
}
