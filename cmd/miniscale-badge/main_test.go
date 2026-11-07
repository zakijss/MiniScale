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

func TestStoppedState(t *testing.T) {
	for _, c := range []struct {
		stat string
		want bool
	}{
		{"733 (MainUI) S 1 733 733 0 -1", false},
		{"733 (MainUI) T 1 733 733 0 -1", true},
		{"733 (MainUI) t 1 733 733 0 -1", true},
		{"733 (MainUI) R 1 733 733 0 -1", false},
		{"9 (odd) T name) S 1 9 9", false}, // the last ")" ends the name
		{"9 (odd) S name) T 1 9 9", true},
		{"733 (MainUI)", false},
		{"", false},
	} {
		if got := stoppedState([]byte(c.stat)); got != c.want {
			t.Errorf("%q: got %v, want %v", c.stat, got, c.want)
		}
	}
}
