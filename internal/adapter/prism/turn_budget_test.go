package prism

import (
	"testing"
	"time"
)

func TestTurnBudgetEnv(t *testing.T) {
	if turnBudget() != pollTurnBudget {
		t.Fatalf("default budget = %v, want %v", turnBudget(), pollTurnBudget)
	}
	t.Setenv("PRISM_TURN_BUDGET_SECONDS", "60")
	if turnBudget() != 60*time.Second {
		t.Fatalf("env override = %v, want 60s", turnBudget())
	}
	t.Setenv("PRISM_TURN_BUDGET_SECONDS", "-5")
	if turnBudget() != pollTurnBudget {
		t.Fatalf("invalid env must fall back, got %v", turnBudget())
	}
}
