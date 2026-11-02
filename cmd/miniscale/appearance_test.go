package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/zakijss/minideck/ui"
)

// presses is input that presses each button in turn, then ends.
func presses(bs ...ui.Button) <-chan ui.Event {
	ch := make(chan ui.Event, len(bs))
	for _, b := range bs {
		ch <- ui.Event{Button: b, Pressed: true}
	}
	close(ch)
	return ch
}

// lookScreen opens a screen that draws nowhere, with the shared look saved
// to a temporary file. The look in use is put back after the test.
func lookScreen(t *testing.T) (*ui.Screen, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "appearance.json")
	t.Setenv("MINIDECK_UI_NULL", "1")
	t.Setenv("MINIDECK_APPEARANCE", path)
	scr, err := ui.Open()
	if err != nil {
		t.Fatal(err)
	}
	start := ui.CurrentAppearance()
	t.Cleanup(func() { scr.SetAppearance(start) })
	return scr, path
}

// wantLookChanged checks that Appearance was opened: RIGHT on its first
// row moved to the next theme, and B saved it.
func wantLookChanged(t *testing.T, start ui.Appearance, path string) {
	t.Helper()
	if ui.CurrentAppearance().Theme == start.Theme {
		t.Errorf("theme still %q: Appearance didn't open", start.Theme)
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("look not saved: %v", err)
	}
}

// The Settings tab ends with Appearance, and choosing it opens the screen.
func TestSettingsTabHasAppearance(t *testing.T) {
	scr, path := lookScreen(t)
	start := ui.CurrentAppearance()
	a := &app{scr: scr, input: presses(ui.Right, ui.B), snap: snapshot{on: true, st: &Status{BackendState: "Running"}}}
	a.tab = tabSettings
	items := a.items(tabSettings)
	last := items[len(items)-1]
	if last.Label != "Appearance" || last.Value != (row{"appearance", ""}) {
		t.Fatalf("Settings end with %+v", last)
	}
	a.lists[tabSettings] = ui.List{Items: items, Sel: len(items) - 1}
	a.activate()
	wantLookChanged(t, start, path)
}

// SELECT opens Appearance in every state, here with MiniScale off.
func TestSelectOpensAppearance(t *testing.T) {
	scr, path := lookScreen(t)
	start := ui.CurrentAppearance()
	a := &app{scr: scr, input: presses(ui.Right, ui.B)}
	if !a.key(ui.Event{Button: ui.Select, Pressed: true}) {
		t.Error("SELECT closed the app")
	}
	wantLookChanged(t, start, path)
}
