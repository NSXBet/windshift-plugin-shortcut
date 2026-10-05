package logic

import (
	"testing"
	"time"
)

var base = time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)

func TestShouldSkip(t *testing.T) {
	cases := []struct {
		name       string
		found      bool
		synced     string
		external   string
		wantSkip   bool
		wantErr    bool
	}{
		{"not mapped", false, "", "2026-10-05T01:00:00Z", false, false},
		{"external unchanged since sync", true, "2026-10-05T02:00:00Z", "2026-10-05T01:00:00Z", true, false},
		{"equal timestamps skip", true, "2026-10-05T02:00:00Z", "2026-10-05T02:00:00Z", true, false},
		{"external newer re-processes", true, "2026-10-05T02:00:00Z", "2026-10-05T03:00:00Z", false, false},
		{"missing external timestamp", true, "2026-10-05T02:00:00Z", "", false, true},
		{"missing synced timestamp", true, "", "2026-10-05T01:00:00Z", false, true},
		{"bad external format", true, "2026-10-05T02:00:00Z", "yesterday", false, true},
		{"bad synced format", true, "not-a-time", "2026-10-05T01:00:00Z", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			skip, err := ShouldSkip(tc.found, tc.synced, tc.external)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err=%v wantErr=%v", err, tc.wantErr)
			}
			if skip != tc.wantSkip {
				t.Fatalf("skip=%v want %v", skip, tc.wantSkip)
			}
		})
	}
}

func TestFreshWindow(t *testing.T) {
	step := 24 * time.Hour

	start, end := FreshWindow(base.Add(-72*time.Hour), base, step)
	if !start.Equal(base.Add(-72 * time.Hour)) || !end.Equal(base.Add(-48*time.Hour)) {
		t.Fatalf("backfill chunk: got [%v, %v)", start, end)
	}

	start, end = FreshWindow(base.Add(-2*time.Hour), base, step)
	if !start.Equal(base.Add(-2 * time.Hour)) || !end.Equal(base) {
		t.Fatalf("tail clamped to now: got [%v, %v)", start, end)
	}

	start, end = FreshWindow(base, base, step)
	if !start.Equal(base) || !end.Equal(base) {
		t.Fatalf("caught-up watermark: got [%v, %v)", start, end)
	}
}

func TestAdvanceWatermark(t *testing.T) {
	windowEnd := base.Add(3 * time.Hour)
	overlap := time.Hour

	got := AdvanceWatermark(windowEnd, overlap)
	if !got.Equal(windowEnd.Add(-overlap)) {
		t.Fatalf("advance=%v want %v", got, windowEnd.Add(-overlap))
	}

	if zero := AdvanceWatermark(windowEnd, 0); !zero.Equal(windowEnd) {
		t.Fatalf("zero overlap should advance exactly to window end, got %v", zero)
	}
}

func TestItemTypeName(t *testing.T) {
	cases := map[string]string{
		"feature":   "Feature",
		"bug":       "Bug",
		"chore":     "Chore",
		" epic ":    "",
		"":          "",
		"sometingt": "",
	}
	for in, want := range cases {
		if got := ItemTypeName(in); got != want {
			t.Errorf("ItemTypeName(%q)=%q want %q", in, got, want)
		}
	}
}

func TestRateLimiter(t *testing.T) {
	lim := NewRateLimiter(3, time.Minute)

	for i := range 3 {
		if !lim.Allow(base.Add(time.Duration(i) * time.Second)) {
			t.Fatalf("slot %d should be allowed", i)
		}
	}
	if lim.Allow(base.Add(3 * time.Second)) {
		t.Fatal("4th call within window should be denied")
	}
	if lim.Used() != 3 {
		t.Fatalf("used=%d want 3", lim.Used())
	}
	// At +60s the sliding window [now-60s, now) still holds +1s and +2s; the
	// +0s entry is exactly at the boundary and drops. So a 4th real call is
	// allowed again and used = 2 kept + 1 new.
	if !lim.Allow(base.Add(time.Minute)) {
		t.Fatal("call at window boundary should be allowed")
	}
	if lim.Used() != 3 {
		t.Fatalf("used after expiry=%d want 3 (2 within window + fresh)", lim.Used())
	}
}

func TestTickBudget(t *testing.T) {
	expired := TickBudget{deadline: base.Add(-time.Second), now: func() time.Time { return base }}
	if expired.Remaining() {
		t.Fatal("expired budget should have no remaining time")
	}
	live := TickBudget{deadline: base.Add(time.Hour), now: func() time.Time { return base }}
	if !live.Remaining() {
		t.Fatal("future budget should have remaining time")
	}
}
