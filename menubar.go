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
	wake     chan struct{}     // signals the poll loop to refresh immediately (e.g. after a swap)
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

	st := &menuState{store: s, notified: map[string]string{}, wake: make(chan struct{}, 1)}

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
		st.maybeSwap(app, results, threshold)
		app.SetMenuState(&menuet.MenuState{Title: st.title()})
		app.MenuChanged()

		select {
		case <-time.After(menubarPollInterval):
		case <-st.wake: // an action (e.g. a swap) asked for an immediate refresh
		}
	}
}

// signalWake asks the poll loop to refresh now, without blocking if a wake is
// already pending.
func (st *menuState) signalWake() {
	select {
	case st.wake <- struct{}{}:
	default:
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

// deferThresholdPct is the second-tier SESSION trigger: when no account is below
// the alert level, the swap waits until the active session is this high (just
// under the 100% API limit) before accepting a less-fresh target.
const deferThresholdPct = 98

// swapDecision is the outcome of evaluating an alert for an auto-swap.
type swapDecision struct {
	target *result // account to install into the keychain, or nil for no swap
	reason string  // why (for logging / diagnostics)
}

// selectSwapTarget decides whether and where to swap when the active account's
// SESSION (5h) usage has crossed the alert threshold. The threshold applies to
// the session window only; weekly (7d) usage is used solely to rank candidates.
func selectSwapTarget(active result, others []result, threshold float64) swapDecision {
	if len(others) == 0 {
		return swapDecision{nil, "no other accounts to swap to"}
	}
	// Tier 1: candidates whose session is below the alert level, lowest weekly.
	if t1 := sessionBelow(others, threshold); len(t1) > 0 {
		return swapDecision{lowestWeekly(t1), "tier1: session below threshold, lowest weekly"}
	}
	// Tier 2: only once the active session is critically high (near the limit).
	if active.usage.FiveHour.Utilization < deferThresholdPct {
		return swapDecision{nil, "no candidate below threshold; deferring until active session hits defer level"}
	}
	if t2 := sessionBelow(others, deferThresholdPct); len(t2) > 0 {
		return swapDecision{lowestWeekly(t2), "tier2: session below defer level, lowest weekly"}
	}
	return swapDecision{nil, "no candidate with session below defer level; no swap"}
}

// previewTargetUUID returns the uuid of the account a swap would currently pick,
// or "" if none qualifies. It mirrors selectSwapTarget's ranking (session below
// the alert level, else below the defer level; lowest weekly) but ignores the
// active account's utilization so the standby target shows before an alert fires.
func previewTargetUUID(results []result, threshold float64) string {
	var others []result
	for i := range results {
		if results[i].err == nil && !results[i].active {
			others = append(others, results[i])
		}
	}
	cands := sessionBelow(others, threshold)
	if len(cands) == 0 {
		cands = sessionBelow(others, deferThresholdPct)
	}
	if len(cands) == 0 {
		return ""
	}
	return lowestWeekly(cands).uuid
}

// sessionBelow returns accounts whose 5h SESSION usage is strictly below cap.
func sessionBelow(rs []result, cap float64) []result {
	var out []result
	for i := range rs {
		if rs[i].usage.FiveHour.Utilization < cap {
			out = append(out, rs[i])
		}
	}
	return out
}

// lowestWeekly picks the account with the lowest 7d WEEKLY usage, ties broken by
// email for determinism.
func lowestWeekly(rs []result) *result {
	best := &rs[0]
	for i := 1; i < len(rs); i++ {
		w, bw := rs[i].usage.SevenDay.Utilization, best.usage.SevenDay.Utilization
		if w < bw || (w == bw && rs[i].email < best.email) {
			best = &rs[i]
		}
	}
	return best
}

// maybeSwap performs an auto-swap when enabled and the active account's session
// has crossed the alert threshold. It refreshes the target's credential, writes
// it into the Claude Code keychain (in place, preserving the ACL), persists the
// refreshed token, and notifies. The running Claude Code session adopts the new
// token on its next request.
func (st *menuState) maybeSwap(app *menuet.Application, results []result, threshold float64) {
	st.mu.RLock()
	enabled := st.store.Settings.AutoSwapOnAlert
	st.mu.RUnlock()
	if !enabled {
		return
	}

	var active *result
	var others []result
	for i := range results {
		if results[i].err != nil {
			continue // unknown usage — never a candidate, and can't trust "active" state
		}
		if results[i].active {
			active = &results[i]
		} else {
			others = append(others, results[i])
		}
	}
	if active == nil || active.usage.FiveHour.Utilization < threshold {
		return
	}

	decision := selectSwapTarget(*active, others, threshold)
	if decision.target == nil {
		return
	}
	st.performSwap(app, active, decision.target)
}

// performSwap refreshes the target account's credential, installs it into the
// keychain, and persists it. On failure it notifies and leaves state untouched
// so the next poll retries.
func (st *menuState) performSwap(app *menuet.Application, from, to *result) {
	st.mu.Lock()
	// Locate the target account in the store by its stable uuid.
	idx := -1
	for i := range st.store.Accounts {
		if st.store.Accounts[i].AccountUUID == to.uuid {
			idx = i
			break
		}
	}
	if idx < 0 {
		st.mu.Unlock()
		return
	}
	cred := st.store.Accounts[idx].Credential
	st.mu.Unlock()

	// Ensure the token we hand off is fresh so the swapped session isn't blocked
	// by an expired access token.
	if nc, err := refresh(cred); err == nil {
		cred = nc
	}

	if err := writeKeychainCredential(cred); err != nil {
		app.Notification(menuet.Notification{
			Title:      "Claude usage — swap failed",
			Subtitle:   fmt.Sprintf("could not switch to %s", to.email),
			Message:    err.Error(),
			Identifier: "claude-usage-swap-error",
		})
		return
	}

	// Persist the refreshed credential.
	st.mu.Lock()
	st.store.Accounts[idx].Credential = cred
	s := st.store
	st.mu.Unlock()
	if err := saveStore(s); err != nil {
		fmt.Fprintln(os.Stderr, "persist after swap:", err)
	}

	// Refresh the display now so the active dot/target move immediately rather
	// than on the next poll tick.
	st.signalWake()

	msg := "Type \"continue\" in Claude Code to resume on the new account."
	if from != nil {
		msg = fmt.Sprintf("%s session hit %.0f%%. ", from.email, from.usage.FiveHour.Utilization) + msg
	}
	app.Notification(menuet.Notification{
		Title:      "Claude account swapped",
		Subtitle:   fmt.Sprintf("→ %s", to.email),
		Message:    msg,
		Identifier: "claude-usage-swap",
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

	// When auto-swap is on, mark the account a swap would currently pick.
	swapTarget := ""
	if st.store.Settings.AutoSwapOnAlert {
		swapTarget = previewTargetUUID(results, threshold)
	}

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
		if swapTarget != "" && r.uuid == swapTarget {
			title = append(title, menuet.TextRun{Text: "  *", FontWeight: menuet.WeightBold, Color: menuet.SystemBlue})
		}
		// A no-op Clicked marks the row clickable so AppKit renders it
		// enabled (full contrast) instead of dimming it like a disabled item.
		items = append(items, menuet.Regular{Runs: title, Clicked: noop})
		if r.err != nil {
			items = append(items, menuet.Regular{Runs: []menuet.TextRun{{Text: r.err.Error(), FontSize: 12, Color: menuet.SystemRed}}, Clicked: noop})
			continue
		}
		items = append(items, menuet.Regular{Runs: windowRuns("Session", "5h", r.usage.FiveHour, now), Clicked: noop})
		items = append(items, menuet.Regular{Runs: windowRuns("Weekly", "7d", r.usage.SevenDay, now), Clicked: noop})
	}

	st.mu.RLock()
	autoSwap := st.store.Settings.AutoSwapOnAlert
	st.mu.RUnlock()

	items = append(items, menuet.Separator{})
	items = append(items, menuet.Regular{
		Text:     fmt.Sprintf("Alert threshold: %.0f%%", threshold),
		Children: func() []menuet.MenuItem { return st.thresholdItems() },
	})
	items = append(items, menuet.Regular{
		Text:    "Enable Auto-swap on alert",
		State:   autoSwap,
		Clicked: st.toggleAutoSwap,
	})
	items = append(items, menuet.Regular{
		Text:     "Swap to",
		Children: func() []menuet.MenuItem { return st.swapMenuItems() },
	})
	return items
}

// swapMenuItems lists every account as a manual swap target. The active account
// is checked and non-clickable; selecting another swaps to it immediately.
func (st *menuState) swapMenuItems() []menuet.MenuItem {
	st.mu.RLock()
	results := st.results
	st.mu.RUnlock()

	sorted := make([]result, len(results))
	copy(sorted, results)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].email < sorted[j].email })

	var items []menuet.MenuItem
	for _, r := range sorted {
		r := r
		if r.active {
			items = append(items, menuet.Regular{Text: r.email, State: true, Clicked: noop})
			continue
		}
		if r.err != nil {
			// Unknown usage/identity — can't safely swap to it.
			items = append(items, menuet.Regular{Text: r.email + " (unavailable)", Clicked: noop})
			continue
		}
		uuid := r.uuid
		items = append(items, menuet.Regular{
			Text:    r.email,
			Clicked: func() { st.swapTo(menuet.App(), uuid) },
		})
	}
	return items
}

// swapTo swaps to the account with the given uuid, regardless of threshold.
func (st *menuState) swapTo(app *menuet.Application, uuid string) {
	st.mu.RLock()
	results := st.results
	st.mu.RUnlock()
	var to, from result
	var haveTo, haveFrom bool
	for i := range results {
		if results[i].uuid == uuid {
			to, haveTo = results[i], true
		}
		if results[i].active {
			from, haveFrom = results[i], true
		}
	}
	if !haveTo {
		return
	}
	var fromPtr *result
	if haveFrom {
		fromPtr = &from
	}
	st.performSwap(app, fromPtr, &to)
}

// toggleAutoSwap flips and persists the auto-swap setting.
func (st *menuState) toggleAutoSwap() {
	st.mu.Lock()
	st.store.Settings.AutoSwapOnAlert = !st.store.Settings.AutoSwapOnAlert
	s := st.store
	st.mu.Unlock()
	if err := saveStore(s); err != nil {
		fmt.Fprintln(os.Stderr, "save settings:", err)
	}
	menuet.App().MenuChanged()
}

// noop is attached to informational rows so AppKit treats them as enabled and
// renders them at full contrast rather than dimming disabled items.
func noop() {}

// windowRuns renders one window on its own line as
// "<Name> (5h)  <pct%> · resets <dur>", with the name spelled out (Session /
// Weekly) so the two windows are unmistakable and the percentage colored by
// severity.
func windowRuns(name, short string, w window, now time.Time) []menuet.TextRun {
	// Pad the name column so the two rows' percentages line up ("Session" is
	// longer than "Weekly").
	label := fmt.Sprintf("%-7s (%s)  ", name, short)
	runs := []menuet.TextRun{
		{Text: label, Color: menuet.LabelSecondary},
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
	for _, pct := range []float64{70, 75, 80, 85, 90, 95, 98} {
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
	results := st.results
	st.mu.Unlock()
	if err := saveStore(s); err != nil {
		fmt.Fprintln(os.Stderr, "save settings:", err)
	}
	// Evaluate a swap immediately rather than waiting for the next poll tick,
	// so lowering the threshold below the active session acts at once.
	st.maybeSwap(menuet.App(), results, pct)
	menuet.App().MenuChanged()
}
