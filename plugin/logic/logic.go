// Package logic holds the sync decision functions for the Shortcut plugin.
// They are pure, stdlib-only, and buildable/testable on darwin — unlike
// package main, which is GOOS=wasip1-only (go:wasmexport). Engine wiring
// (plugin main) injects time.Now so these stay deterministic in tests.
package logic

import (
	"fmt"
	"strings"
	"time"
	"unicode"
)

// TickBudget bounds one sync tick. The wasm invocation deadline is 5s
// (contract §2/§6); callers reserve ~800ms for the final state save. The
// clock is injectable for tests; time.Now works in the guest (WASI enabled).
type TickBudget struct {
	deadline time.Time
	now      func() time.Time
}

func NewTickBudget(start time.Time, dur time.Duration) TickBudget {
	return TickBudget{deadline: start.Add(dur), now: time.Now}
}

func (b TickBudget) Remaining() bool { return b.now().Before(b.deadline) }

// DeadlineMs reports the milliseconds left until the tick deadline. A fetch
// must clamp its HTTP timeout to this minus a margin, or an in-flight call
// outlives the wasm invocation cap and the module is killed mid-tick.
func (b TickBudget) DeadlineMs() int64 {
	return b.deadline.Sub(b.now()).Milliseconds()
}

// ShouldSkip is the idempotency guard (contract §6): skip when a mapped item
// exists and was last synced at/after the Shortcut object's updated_at.
// Parse errors are returned, never swallowed — overwriting on a bad
// timestamp would corrupt the one-way sync.
func ShouldSkip(found bool, lastSyncedAt, externalUpdatedAt string) (bool, error) {
	if !found {
		return false, nil
	}
	if lastSyncedAt == "" || externalUpdatedAt == "" {
		return false, fmt.Errorf("shouldSkip: missing timestamp (synced=%q external=%q)", lastSyncedAt, externalUpdatedAt)
	}
	synced, err := time.Parse(time.RFC3339, lastSyncedAt)
	if err != nil {
		return false, fmt.Errorf("shouldSkip: last_synced_at %q: %w", lastSyncedAt, err)
	}
	ext, err := time.Parse(time.RFC3339, externalUpdatedAt)
	if err != nil {
		return false, fmt.Errorf("shouldSkip: external_updated_at %q: %w", externalUpdatedAt, err)
	}
	return !synced.Before(ext), nil
}

// FreshWindow picks bounds for a new processing window from the watermark:
// [wm, min(wm+step, now)). Bounds are frozen in state once picked and reused
// across ticks until the window completes — Shortcut stories/search has no
// pagination (plain array, contract §4), so resume is via a persisted index
// into the fetched array, which only stays valid while bounds are fixed.
func FreshWindow(wm, now time.Time, step time.Duration) (start, end time.Time) {
	end = wm.Add(step)
	if end.After(now) {
		end = now
	}
	return wm, end
}

// AdvanceWatermark slides the cursor past a completed window, keeping
// `overlap` re-processed as boundary insurance: updates racing the window's
// now-read land in the next window and are skipped by the no-op guard.
func AdvanceWatermark(windowEnd time.Time, overlap time.Duration) time.Time {
	return windowEnd.Add(-overlap)
}

// StatusNameForStateType maps a Shortcut workflow-state type to the Windshift
// status name. Windshift's default schema has exactly three statuses
// (Open / In Progress / Done), while Shortcut state names are
// workspace-customizable ("Completed", "Ready for Deploy", …) so names can't
// be passed through — the state's type is the stable signal. Unknown types
// return "" (no status field is sent; core keeps its default).
func StatusNameForStateType(stateType string) string {
	switch stateType {
	case "backlog", "unstarted":
		return "Open"
	case "started":
		return "In Progress"
	case "done":
		return "Done"
	default:
		return ""
	}
}

// titleLine normalizes a Shortcut name to the single-line form the core
// accepts in a title (or a markdown heading): newlines, tabs, and control
// runes collapse to single spaces, then the result is trimmed.
func titleLine(title string) string {
	var b strings.Builder
	b.Grow(len(title))
	for _, r := range title {
		if r == '\n' || r == '\r' || unicode.IsControl(r) {
			b.WriteRune(' ')
		} else {
			b.WriteRune(r)
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// truncateRunes cuts s to at most max runes without splitting a rune.
func truncateRunes(s string, max int) string {
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max])
}

// TitleAndDescription adapts a Shortcut name/description pair to the core's
// title rules (validation.NormalizeTitle: trimmed, ≤255 runes, single line,
// no control runes). A migration must not drop an item over cosmetic
// formatting, so names are cleaned and — when they must be cut — the full
// original name is preserved by prepending it to the description as a
// markdown heading, keeping whatever description exists below it:
//
//	# <full original name>
//
//	<description>
//
// Names that fit after cleanup pass through with the description untouched.
func TitleAndDescription(name, description string) (string, string) {
	full := titleLine(name)
	title := truncateRunes(full, 255)
	if title == full {
		return title, description
	}
	heading := "# " + full + "\n\n"
	if description == "" {
		return title, heading
	}
	return title, heading + description
}

// RateLimiter is a per-instance sliding-window budget. A fresh WASM instance
// is created per invocation, so this only guards bursts within one tick; the
// host-side timeout bounds each call. 429 responses are never retried
// in-wasm — the tick aborts and the next scheduled tick resumes (no sleep
// primitive in wasm).
type RateLimiter struct {
	max    int
	window time.Duration
	seen   []time.Time
}

func NewRateLimiter(max int, window time.Duration) *RateLimiter {
	return &RateLimiter{max: max, window: window}
}

// Allow reserves a slot at now, dropping expired entries first.
func (r *RateLimiter) Allow(now time.Time) bool {
	keep := r.seen[:0]
	for _, t := range r.seen {
		if now.Sub(t) < r.window {
			keep = append(keep, t)
		}
	}
	r.seen = keep
	if len(r.seen) >= r.max {
		return false
	}
	r.seen = append(r.seen, now)
	return true
}

// Used reports how many slots are currently consumed.
func (r *RateLimiter) Used() int { return len(r.seen) }
