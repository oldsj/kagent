package driver

import (
	"testing"
	"time"
)

func TestActiveBudgetRetainsElapsedTimeAcrossApprovals(t *testing.T) {
	budget := newActiveBudget(time.Hour)
	defer budget.pause()
	budget.started = budget.started.Add(-20 * time.Minute)
	budget.pause()
	if budget.done() != nil || budget.expired() {
		t.Fatal("paused budget can expire")
	}
	if budget.remaining > 40*time.Minute || budget.remaining < 39*time.Minute {
		t.Fatalf("remaining budget = %s", budget.remaining)
	}
	remaining := budget.remaining
	budget.pause()
	if budget.remaining != remaining {
		t.Fatal("repeated pause charged approval time")
	}
	budget.resume()
	started := budget.started
	budget.resume()
	if budget.started != started {
		t.Fatal("repeated resume reset elapsed execution")
	}
	budget.started = budget.started.Add(-10 * time.Minute)
	budget.pause()
	if budget.remaining > 30*time.Minute || budget.remaining < 29*time.Minute {
		t.Fatalf("remaining budget after second approval = %s", budget.remaining)
	}
	budget.resume()
	budget.started = budget.started.Add(-time.Hour)
	if !budget.expired() {
		t.Fatal("cumulative active time did not exhaust budget")
	}
	budget.pause()
	budget.resume()
	select {
	case <-budget.done():
	case <-time.After(time.Second):
		t.Fatal("exhausted budget did not fire on resume")
	}
}

func TestAbsentBudgetIsInactive(t *testing.T) {
	var budget *activeBudget
	budget.pause()
	budget.resume()
	if budget.done() != nil || budget.expired() {
		t.Fatal("absent post-result budget is active")
	}
}
