package main

import (
	"testing"
	"time"

	"ocr/internal/config"
)

// Unit tests for the pure stall-alert throttle policy behind the silent-stall
// watchdog. The DB-touching watchdogCheck delegates its decision to
// stallAlertDecision, so the throttle behaviour is verified here without a DB,
// clock, or Telegram.

func TestStallAlertDecision(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	threshold := 15 * time.Minute

	t.Run("no data is not a stall", func(t *testing.T) {
		alert, last := stallAlertDecision(time.Time{}, false, now, threshold, time.Time{})
		if alert {
			t.Fatal("no data must not alert")
		}
		if !last.IsZero() {
			t.Fatal("no data must arm the throttle (zero last)")
		}
	})

	t.Run("fresh within threshold is healthy", func(t *testing.T) {
		freshest := now.Add(-5 * time.Minute) // well within 15m
		alert, last := stallAlertDecision(freshest, true, now, threshold, time.Time{})
		if alert {
			t.Fatal("activity within threshold must not alert")
		}
		if !last.IsZero() {
			t.Fatal("healthy state must clear the throttle")
		}
	})

	t.Run("first stall crossing alerts", func(t *testing.T) {
		freshest := now.Add(-30 * time.Minute) // beyond 15m
		alert, last := stallAlertDecision(freshest, true, now, threshold, time.Time{})
		if !alert {
			t.Fatal("first crossing past threshold must alert")
		}
		if !last.Equal(now) {
			t.Fatalf("last-alert = %v, want %v", last, now)
		}
	})

	t.Run("sustained stall is throttled within the re-alert window", func(t *testing.T) {
		freshest := now.Add(-30 * time.Minute)
		// Alerted 10 minutes ago; re-alert window is 1h, so still suppressed.
		lastAlert := now.Add(-10 * time.Minute)
		alert, last := stallAlertDecision(freshest, true, now, threshold, lastAlert)
		if alert {
			t.Fatal("a sustained stall inside the re-alert window must be throttled")
		}
		if !last.Equal(lastAlert) {
			t.Fatalf("throttled decision must carry the existing lastAlert forward (got %v, want %v)", last, lastAlert)
		}
	})

	t.Run("sustained stall re-alerts after the window", func(t *testing.T) {
		freshest := now.Add(-90 * time.Minute)
		// Alerted over an hour ago: the re-alert window has elapsed.
		lastAlert := now.Add(-stallReAlertInterval - time.Minute)
		alert, last := stallAlertDecision(freshest, true, now, threshold, lastAlert)
		if !alert {
			t.Fatal("a stall must re-alert once the re-alert window elapses")
		}
		if !last.Equal(now) {
			t.Fatalf("re-alert must reset last-alert to now (got %v, want %v)", last, now)
		}
	})

	t.Run("recovery after an alert clears the throttle", func(t *testing.T) {
		freshest := now.Add(-1 * time.Minute) // recovered
		lastAlert := now.Add(-5 * time.Minute)
		alert, last := stallAlertDecision(freshest, true, now, threshold, lastAlert)
		if alert {
			t.Fatal("recovered activity must not alert")
		}
		if !last.IsZero() {
			t.Fatal("recovery must clear the throttle so the next stall alerts immediately")
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
