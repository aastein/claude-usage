package main

import (
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/caseymrm/menuet/v2"
)

// menuState is the mutex-guarded snapshot the poll goroutine writes and the
// menu-rendering callbacks read.
type menuState struct {
	mu       sync.RWMutex
	store    *store
	results  []result
	updated  time.Time
	polling  bool
	notified map[string]string // window key -> ResetsAt that last fired a notification
}

// cmdMenubar runs the macOS menu bar app: a "👾 NN%" title showing the active
// account's 5-hour utilization, a dropdown with every account's full usage, an
// alert-threshold submenu, and a notification when the active account crosses
// the threshold on either window.
func cmdMenubar() {
	// menuet initializes UNUserNotificationCenter at startup, which macOS
	// aborts unless the process runs inside a .app bundle. Refuse early with a
	// useful message instead of crashing with an NSException.
	if !runningInBundle() {
		fmt.Fprintln(os.Stderr, "The menu bar app must run from a .app bundle, not the bare binary.")
		fmt.Fprintln(os.Stderr, "Build and launch it with:")
		fmt.Fprintln(os.Stderr, "  make run-bundle        # or: make install-bundle, then open from Spotlight")
		os.Exit(1)
	}

	s, err := loadStore()
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	if len(s.Accounts) == 0 {
		fmt.Fprintln(os.Stderr, "no accounts configured — run: claude-usage login")
		os.Exit(1)
	}
	normalizeStore(s)

	st := &menuState{store: s, notified: map[string]string{}}

	app := menuet.App()
	app.Name = "Claude Usage"
	app.Label = "com.aastein.claude-usage"
	app.Children = st.menuItems

	go st.pollLoop(app)

	app.RunApplication()
}

// pollLoop refreshes usage immediately and then every pollInterval, updating the
// title, redrawing the menu, and firing notifications.
func (st *menuState) pollLoop(app *menuet.Application) {
	for {
		st.mu.Lock()
		st.polling = true
		s := st.store
		st.mu.Unlock()
		app.SetMenuState(&menuet.MenuState{Title: st.title()})

		results := poll(s)

		st.mu.Lock()
		st.results = results
		st.updated = time.Now()
		st.polling = false
		threshold := s.Settings.NotifyThresholdPct
		st.mu.Unlock()

		st.maybeNotify(app, results, threshold)
		app.SetMenuState(&menuet.MenuState{Title: st.title()})
		app.MenuChanged()

		time.Sleep(pollInterval)
	}
}

// activeResult returns the currently-active account's result, or nil.
func (st *menuState) activeResult() *result {
	for i := range st.results {
		if st.results[i].active {
			return &st.results[i]
		}
	}
	return nil
}

// title renders the menu bar title: "👾 NN%" from the active account's 5-hour
// utilization. Falls back gracefully before the first poll or with no active
// account.
func (st *menuState) title() string {
	st.mu.RLock()
	defer st.mu.RUnlock()
	if st.polling && st.results == nil {
		return "👾 …"
	}
	a := st.activeResult()
	if a == nil || a.err != nil {
		return "👾 —"
	}
	return fmt.Sprintf("👾 %.0f%%", a.usage.FiveHour.Utilization)
}

// maybeNotify fires a notification for the active account when either window is
// at or above the threshold, de-duplicated per reset window so it fires once per
// window rather than every poll.
func (st *menuState) maybeNotify(app *menuet.Application, results []result, threshold float64) {
	var a *result
	for i := range results {
		if results[i].active {
			a = &results[i]
			break
		}
	}
	if a == nil || a.err != nil {
		return
	}
	st.checkWindow(app, "5-hour session", a.usage.FiveHour, threshold)
	st.checkWindow(app, "7-day", a.usage.SevenDay, threshold)
}

func (st *menuState) checkWindow(app *menuet.Application, label string, w window, threshold float64) {
	if w.Utilization < threshold {
		// Below threshold: clear any prior notification state for this window
		// so a fresh crossing later re-fires.
		st.mu.Lock()
		delete(st.notified, label)
		st.mu.Unlock()
		return
	}
	st.mu.Lock()
	already := st.notified[label] == w.ResetsAt
	if !already {
		st.notified[label] = w.ResetsAt
	}
	st.mu.Unlock()
	if already {
		return
	}
	app.Notification(menuet.Notification{
		Title:      "Claude usage alert",
		Subtitle:   fmt.Sprintf("%s limit at %.0f%%", label, w.Utilization),
		Message:    "Active Claude Code account has reached the alert threshold.",
		Identifier: "claude-usage-" + label,
	})
}

// menuItems builds the dropdown: every account with both windows, then the
// alert-threshold submenu. menuet auto-appends "Start at Login" and "Quit".
func (st *menuState) menuItems() []menuet.MenuItem {
	st.mu.RLock()
	results := st.results
	updated := st.updated
	polling := st.polling
	threshold := st.store.Settings.NotifyThresholdPct
	st.mu.RUnlock()

	now := time.Now()
	items := []menuet.MenuItem{}

	head := "Updating…"
	if !updated.IsZero() {
		head = "Updated " + updated.Format("3:04 PM")
		if polling {
			head += "  ·  refreshing…"
		}
	}
	items = append(items, menuet.Regular{
		Runs: []menuet.TextRun{{Text: head, FontSize: 12, Color: menuet.LabelSecondary}},
	})
	items = append(items, menuet.Separator{})

	sorted := make([]result, len(results))
	copy(sorted, results)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].email < sorted[j].email })

	for i, r := range sorted {
		if i > 0 {
			items = append(items, menuet.Separator{})
		}
		title := []menuet.TextRun{}
		if r.active {
			title = append(title, menuet.TextRun{Text: "● ", Color: menuet.Green, FontWeight: menuet.WeightBold})
		}
		title = append(title, menuet.TextRun{Text: r.email, FontWeight: menuet.WeightSemibold, Color: menuet.LabelPrimary})
		if r.active {
			title = append(title, menuet.TextRun{Text: "  active", FontSize: 11, Color: menuet.SystemGreen})
		}
		// A no-op Clicked marks the row clickable so AppKit renders it
		// enabled (full contrast) instead of dimming it like a disabled item.
		items = append(items, menuet.Regular{Runs: title, Clicked: noop})
		if r.err != nil {
			items = append(items, menuet.Regular{Runs: []menuet.TextRun{{Text: r.err.Error(), FontSize: 12, Color: menuet.SystemRed}}, Clicked: noop})
			continue
		}
		items = append(items, menuet.Regular{Runs: usageRuns(r.usage, now), Clicked: noop})
	}

	items = append(items, menuet.Separator{})
	items = append(items, menuet.Regular{
		Text:     fmt.Sprintf("Alert threshold: %.0f%%", threshold),
		Children: func() []menuet.MenuItem { return st.thresholdItems() },
	})
	return items
}

// noop is attached to informational rows so AppKit treats them as enabled and
// renders them at full contrast rather than dimming disabled items.
func noop() {}

// usageRuns renders both windows on one compact, color-coded line:
// "5h 54% · resets 3h52m    7d 11% · resets 6d16h".
func usageRuns(u usageResponse, now time.Time) []menuet.TextRun {
	runs := windowRuns("5h", u.FiveHour, now)
	runs = append(runs, menuet.TextRun{Text: "      "})
	runs = append(runs, windowRuns("7d", u.SevenDay, now)...)
	return runs
}

// windowRuns renders one window as "<label> <pct%> · resets <dur>" with the
// percentage colored by severity.
func windowRuns(label string, w window, now time.Time) []menuet.TextRun {
	runs := []menuet.TextRun{
		{Text: label + " ", Color: menuet.LabelSecondary},
		{Text: fmt.Sprintf("%.0f%%", w.Utilization), FontWeight: menuet.WeightBold, Color: severityColor(w.Utilization)},
	}
	if t, err := time.Parse(time.RFC3339, w.ResetsAt); err == nil {
		if d := t.Sub(now); d > 0 {
			runs = append(runs, menuet.TextRun{Text: " · resets " + shortDur(d), Color: menuet.LabelSecondary})
		}
	}
	return runs
}

// severityColor mirrors the dashboard bar coloring, using semantic system
// colors that stay legible in light and dark mode.
func severityColor(util float64) menuet.Color {
	switch {
	case util >= 90:
		return menuet.SystemRed
	case util >= 70:
		return menuet.SystemYellow
	default:
		return menuet.SystemGreen
	}
}

// thresholdItems is the alert-threshold submenu. Selecting a value persists it.
func (st *menuState) thresholdItems() []menuet.MenuItem {
	st.mu.RLock()
	current := st.store.Settings.NotifyThresholdPct
	st.mu.RUnlock()
	var items []menuet.MenuItem
	for _, pct := range []float64{75, 80, 85, 90, 95} {
		pct := pct
		items = append(items, menuet.Regular{
			Text:    fmt.Sprintf("%.0f%%", pct),
			State:   pct == current,
			Clicked: func() { st.setThreshold(pct) },
		})
	}
	return items
}

// setThreshold updates and persists the notification threshold, then redraws.
func (st *menuState) setThreshold(pct float64) {
	st.mu.Lock()
	st.store.Settings.NotifyThresholdPct = pct
	// A changed threshold re-arms all windows.
	st.notified = map[string]string{}
	s := st.store
	st.mu.Unlock()
	if err := saveStore(s); err != nil {
		fmt.Fprintln(os.Stderr, "save settings:", err)
	}
	menuet.App().MenuChanged()
}
