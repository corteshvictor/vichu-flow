package core

import (
	"math"
	"testing"
)

// TestTokensTotalSaturates: the per-dimension counters clamp at MaxInt, so the total must
// saturate too — a plain int add of two MaxInt values wraps to negative, resetting the run's
// total below its cap.
func TestTokensTotalSaturates(t *testing.T) {
	b := BudgetState{TokensInSpent: math.MaxInt, TokensOutSpent: math.MaxInt}
	if got := b.TokensTotalSpent(); got != math.MaxInt {
		t.Fatalf("total of two MaxInt counters must saturate at MaxInt, got %d", got)
	}
	b.StageTokensIn = map[string]int{"verify": math.MaxInt}
	b.StageTokensOut = map[string]int{"verify": math.MaxInt}
	if got := b.StageTokensTotal("verify"); got != math.MaxInt {
		t.Fatalf("stage total must saturate at MaxInt, got %d", got)
	}
}
