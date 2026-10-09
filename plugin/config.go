// Operator configuration and resumable sync state, both persisted in the
// per-plugin KV store (contract §3). Token/webhook_secret are write-only:
// no GET route ever echoes them back.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
)

// KV key namespace (contract §3). Tombstones and comment-dedup records are
// opaque to the config/state; only key shapes are fixed here.
const (
	kvConfigKey     = "shortcut:config"
	kvStateKey      = "shortcut:state"
	kvCommentPrefix = "shortcut:cmt:"  // + comment id → synced comment marker
	kvTombPrefix    = "shortcut:tomb:" // + story/epic id → deleted marker
)

// Config is operator-facing configuration (KV key shortcut:config).
type Config struct {
	Token         string  `json:"token"`
	Enabled       bool    `json:"enabled"`
	DryRun        bool    `json:"dry_run"`
	WorkspaceID   string  `json:"workspace_id"`
	WorkspaceMode string  `json:"workspace_mode,omitempty"` // "single" (default) | "per_project"
	ProjectIDs    []int64 `json:"project_ids,omitempty"`
	LabelMode     string  `json:"label_mode"` // "merge" (default) | "replace"
	ActorUserID   int64   `json:"actor_user_id,omitempty"`
	WebhookSecret string  `json:"webhook_secret,omitempty"`
	BackfillDays  int     `json:"backfill_days,omitempty"` // 0 → default 30
}

// Sync phase values for State.Phase.
const (
	phaseCatalog = "catalog"
	phaseStories = "stories"
)

// Counts aggregates one-way sync outcomes for the admin panel.
type Counts struct {
	Created  int `json:"created"`
	Updated  int `json:"updated"`
	Skipped  int `json:"skipped"`
	Errors   int `json:"errors"`
	Comments int `json:"comments"`
}

// State is the resumable sync cursor (KV key shortcut:state).
//
// Story windows: StoryWindowStart/End are FROZEN while in progress
// (stories/search has no pagination, so resume is StoryIdx into the fetched
// array; fixed bounds keep the array stable across ticks). After the array
// is exhausted, the window advances: watermark = end - overlap, then a fresh
// window is picked (contract §4/§6).
type State struct {
	Phase            string `json:"phase"`
	StoryWindowStart string `json:"story_window_start,omitempty"` // RFC3339; window = [start, end)
	StoryWindowEnd   string `json:"story_window_end,omitempty"`
	// WindowStories freezes the open window's search result as condensed
	// story metas (condenseStories). Re-searching every tick was the
	// freeze bug: maps + search blew the wasm cap, the module died before
	// the state save, and the freshly opened window was lost each tick.
	WindowStories string `json:"window_stories,omitempty"`
	// MapsJSON caches the container maps (workflow/epic-workflow state
	// types, project names) so a resumed tick costs zero Shortcut fetches.
	// Refreshed only at reset; post-reset drift degrades to the same
	// unknown-state fallback as an unknown workflow state id.
	MapsJSON       string `json:"maps_json,omitempty"`
	StoryIdx       int    `json:"story_idx,omitempty"`        // index of next unprocessed story in the frozen window
	CatalogEpicIdx int    `json:"catalog_epic_idx,omitempty"` // index of next unprocessed epic in the frozen catalog array
	// CatalogEpics freezes the /epics stream as condensed (id, updated_at)
	// metas, built across ticks by condenseEpics. Re-streaming the ~4 MB
	// payload on every tick starved the budget (fetch ~2.5s + full-struct
	// decode ~4ms/epic left single-digit epics of headroom per tick), so
	// the catalog now runs from state: lookup skips, lazy getEpic for
	// changed bodies.
	CatalogEpics     string   `json:"catalog_epics,omitempty"`
	CatalogCondensed bool     `json:"catalog_condensed,omitempty"`
	LastWindowEnd    string   `json:"last_window_end,omitempty"` // RFC3339; watermark source for the next window
	CommentIdx       int      `json:"comment_idx,omitempty"`     // index of next unprocessed story in the comment pass
	StorySweepIdx    int      `json:"story_sweep_idx,omitempty"` // index of next story in the end-of-window deletion sweep
	Counts           Counts   `json:"counts"`
	LastErrors       []string `json:"last_errors,omitempty"`
}

func loadConfig() (*Config, error) {
	raw, ok, err := kvGetString(kvConfigKey)
	if err != nil || !ok || raw == "" {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	return &c, nil
}

func saveConfig(c *Config) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return kvSetString(kvConfigKey, string(b))
}

func loadState() (*State, error) {
	raw, ok, err := kvGetString(kvStateKey)
	if err != nil || !ok || raw == "" {
		return nil, err
	}
	var s State
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil, fmt.Errorf("state: %w", err)
	}
	if s.Phase == "" {
		s.Phase = phaseCatalog
	}
	return &s, nil
}

func saveState(s *State) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return kvSetString(kvStateKey, string(b))
}

// validate normalizes defaults and rejects unusable configs. Defaults are
// applied to the receiver so the caller can persist the normalized value.
func (c *Config) validate() error {
	if c.Token == "" {
		return errors.New("token is required")
	}
	if c.WorkspaceID == "" {
		// Epics always target this workspace (they are org-level), and it
		// is the fallback for stories in projects missing from /projects.
		return errors.New("workspace_id is required")
	}
	switch c.WorkspaceMode {
	case "":
		c.WorkspaceMode = "single"
	case "single":
	case "per_project":
		// Stories get their own workspace per Shortcut project via
		// workspace_upsert (find-or-create by project id). workspace_id is
		// the epic home plus the unknown-project fallback.
	case "team":
		// Stories get their own workspace per Shortcut team (the org's
		// actual unit of navigation). Each team workspace carries the
		// team's workflow (workspace_upsert states), and story placement
		// comes from the story's workflow state → workflow → team.
		// workspace_id is the epic home plus the unknown-workflow fallback.
	default:
		return errors.New("workspace_mode must be \"single\", \"per_project\" or \"team\"")
	}
	if c.LabelMode == "" {
		c.LabelMode = "merge"
	}
	if c.LabelMode != "merge" && c.LabelMode != "replace" {
		return fmt.Errorf("label_mode must be merge or replace, got %q", c.LabelMode)
	}
	if c.BackfillDays == 0 {
		c.BackfillDays = 30
	}
	if c.BackfillDays < 0 || c.BackfillDays > 4000 {
		return fmt.Errorf("backfill_days must be 1..4000, got %d", c.BackfillDays)
	}
	if c.Enabled && !c.DryRun && c.ActorUserID == 0 {
		return errors.New("actor_user_id is required when enabled (comments author)")
	}
	return nil
}

// key builders — single place so callers can't drift from the namespace.

func commentKey(commentID int64) string {
	return kvCommentPrefix + strconv.FormatInt(commentID, 10)
}

func tombstoneKey(kind string, id int64) string {
	return kvTombPrefix + kind + ":" + strconv.FormatInt(id, 10)
}
