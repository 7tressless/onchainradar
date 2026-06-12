package main

import (
	"testing"
	"time"

	"ocr/internal/config"
)

// Unit tests for the silent-stall watchdog's pure logic: watchdogStep (collector-cursor
// liveness) and stallAlertDecision (the throttle). The DB-touching watchdogCheck only
// reads the cursor and delegates here, so both are verified without a DB, clock, or
// Telegram.

func TestStallAlertDecision(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	threshold := 15 * time.Minute

	t.Run("frozen within threshold is healthy", func(t *testing.T) {
		alert, last := stallAlertDecision(5*time.Minute, threshold, now, time.Time{})
		if alert {
			t.Fatal("a freeze within threshold must not alert")
		}
		if !last.IsZero() {
			t.Fatal("healthy state must clear the throttle")
		}
	})

	t.Run("first stall crossing alerts", func(t *testing.T) {
		alert, last := stallAlertDecision(30*time.Minute, threshold, now, time.Time{})
		if !alert {
			t.Fatal("first crossing past threshold must alert")
		}
		if !last.Equal(now) {
			t.Fatalf("last-alert = %v, want %v", last, now)
		}
	})

	t.Run("sustained stall is throttled within the re-alert window", func(t *testing.T) {
		// Alerted 10 minutes ago; re-alert window is 1h, so still suppressed.
		lastAlert := now.Add(-10 * time.Minute)
		alert, last := stallAlertDecision(30*time.Minute, threshold, now, lastAlert)
		if alert {
			t.Fatal("a sustained stall inside the re-alert window must be throttled")
		}
		if !last.Equal(lastAlert) {
			t.Fatalf("throttled decision must carry the existing lastAlert forward (got %v, want %v)", last, lastAlert)
		}
	})

	t.Run("sustained stall re-alerts after the window", func(t *testing.T) {
		lastAlert := now.Add(-stallReAlertInterval - time.Minute)
		alert, last := stallAlertDecision(90*time.Minute, threshold, now, lastAlert)
		if !alert {
			t.Fatal("a stall must re-alert once the re-alert window elapses")
		}
		if !last.Equal(now) {
			t.Fatalf("re-alert must reset last-alert to now (got %v, want %v)", last, now)
		}
	})
}

func TestWatchdogStep(t *testing.T) {
	threshold := 15 * time.Minute
	t0 := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)

	t.Run("no cursor yet cannot assess", func(t *testing.T) {
		next, alert := watchdogStep(watchdogState{}, 0, false, t0, threshold)
		if alert {
			t.Fatal("a missing cursor must not alert")
		}
		if !next.advancedAt.IsZero() {
			t.Fatal("a missing cursor must leave the state unset")
		}
	})

	t.Run("first observation records progress", func(t *testing.T) {
		next, alert := watchdogStep(watchdogState{}, 100, true, t0, threshold)
		if alert || next.lastBlock != 100 || !next.advancedAt.Equal(t0) {
			t.Fatalf("first observation must record cursor+time without alerting, got %+v alert=%v", next, alert)
		}
	})

	t.Run("advancing cursor stays healthy and clears the throttle", func(t *testing.T) {
		st := watchdogState{lastBlock: 100, advancedAt: t0, lastAlert: t0}
		next, alert := watchdogStep(st, 150, true, t0.Add(5*time.Minute), threshold)
		if alert {
			t.Fatal("an advancing cursor must not alert")
		}
		if next.lastBlock != 150 || !next.lastAlert.IsZero() {
			t.Fatalf("an advance must record the new block and clear lastAlert, got %+v", next)
		}
	})

	t.Run("frozen under threshold preserves the freeze origin", func(t *testing.T) {
		st := watchdogState{lastBlock: 100, advancedAt: t0}
		next, alert := watchdogStep(st, 100, true, t0.Add(10*time.Minute), threshold)
		if alert {
			t.Fatal("frozen under threshold must not alert")
		}
		if !next.advancedAt.Equal(t0) || next.lastBlock != 100 {
			t.Fatalf("frozen must preserve the freeze origin, got %+v", next)
		}
	})

	t.Run("frozen past threshold alerts once then throttles", func(t *testing.T) {
		st := watchdogState{lastBlock: 100, advancedAt: t0}
		now := t0.Add(20 * time.Minute)
		next, alert := watchdogStep(st, 100, true, now, threshold)
		if !alert {
			t.Fatal("a cursor frozen past threshold must alert")
		}
		if !next.lastAlert.Equal(now) || next.lastBlock != 100 || !next.advancedAt.Equal(t0) {
			t.Fatalf("an alert must stamp lastAlert and keep the freeze origin, got %+v", next)
		}
		if _, again := watchdogStep(next, 100, true, now.Add(5*time.Minute), threshold); again {
			t.Fatal("a second tick inside the re-alert window must be throttled")
		}
	})

	t.Run("recovery after a frozen alert clears the throttle", func(t *testing.T) {
		st := watchdogState{lastBlock: 100, advancedAt: t0, lastAlert: t0.Add(20 * time.Minute)}
		next, alert := watchdogStep(st, 200, true, t0.Add(25*time.Minute), threshold)
		if alert {
			t.Fatal("a recovered (advancing) cursor must not alert")
		}
		if !next.lastAlert.IsZero() {
			t.Fatal("recovery must clear the throttle")
		}
	})
}

// TestRawLogsRetention verifies the retention window is the baseline window times
// the safety factor, expressed as a duration.
func TestRawLogsRetention(t *testing.T) {
	cfg := &config.Config{BaselineBuckets: 576, BucketMin: 5}
	got := rawLogsRetention(cfg)
	// 576 * 5 * 2 minutes = 5760 minutes = 96h.
	want := time.Duration(576*5*rawLogsRetentionFactor) * time.Minute
	if got != want {
		t.Fatalf("rawLogsRetention = %s, want %s", got, want)
	}
	if got != 96*time.Hour {
		t.Fatalf("rawLogsRetention = %s, want 96h", got)
	}
}
