package main

import (
	"testing"
	"time"
)

func TestGuardEvery(t *testing.T) {
	now := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	var never time.Time
	for _, c := range []struct {
		name             string
		hotUntil, anchor time.Time
		want             time.Duration
	}{
		{"right after a press", now.Add(time.Second), never, fastGuard},
		{"press moment just over", now, never, slowGuard},
		{"never pressed, no repaint seen", never, never, slowGuard},
		{"just after the menu's repaint", never, now.Add(-10 * time.Millisecond), slowGuard},
		{"mid-second", never, now.Add(-500 * time.Millisecond), slowGuard},
		{"repaint nearly due", never, now.Add(-(tickEvery - early)), fastGuard},
		{"repaint due", never, now.Add(-tickEvery), fastGuard},
		{"repaint a little late", never, now.Add(-(tickEvery + late - time.Millisecond)), fastGuard},
		{"repaint never came", never, now.Add(-(tickEvery + late)), slowGuard},
	} {
		if got := guardEvery(now, c.hotUntil, c.anchor); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}
