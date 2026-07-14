package i18n

import (
	"strings"
	"testing"
)

// TestDriverTokenGuidanceRecommendsStdin: the human-facing token message must steer users to
// --driver-token-stdin, never to the argv form that leaks via `ps`.
func TestDriverTokenGuidanceRecommendsStdin(t *testing.T) {
	msg := catalog["run.driver_token"]
	for _, s := range []string{msg.en, msg.es} {
		if !strings.Contains(s, "--driver-token-stdin") {
			t.Errorf("token guidance must recommend --driver-token-stdin, got: %s", s)
		}
		if strings.Contains(s, "as --driver-token,") || strings.Contains(s, "como --driver-token,") {
			t.Errorf("token guidance must NOT recommend the argv form, got: %s", s)
		}
	}
}
