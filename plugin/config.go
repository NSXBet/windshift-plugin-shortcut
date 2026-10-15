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
	kvCommentPrefix = "shortcut:cmt:"        // + comment id → synced comment marker
	kvTombPrefix    = "shortcut:tomb:"       // + story/epic id → deleted marker
	kvDumpKey       = "shortcut:dump"        // dump state
	kvDumpPiece     = "shortcut:dump:piece:" // + piece no → NDJSON dump piece
	kvRestoreKey    = "shortcut:restore"     // restore state
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

func dumpPieceKey(n int) string {
	return kvDumpPiece + strconv.Itoa(n)
}

// --- dump / restore state (contract §3 shapes, dump/restore feature) ---

// Dump phases.
const (
	dumpPhaseMeta       = "meta"
	dumpPhaseTeams      = "teams"
	dumpPhaseWorkflows  = "workflows"
	dumpPhaseEpicWfs    = "epic_workflows"
	dumpPhaseProjects   = "projects"
	dumpPhaseMilestones = "milestones"
	dumpPhaseEpics      = "epics"
	dumpPhaseStories    = "stories"
	dumpPhaseDone       = "done"
)

// DumpCounts tallies one dump; the status report reads it and the file's
// end line carries it for restore-side sanity checks.
type DumpCounts struct {
	Members       int `json:"members"`
	Teams         int `json:"teams"`
	Workflows     int `json:"workflows"`
	EpicWorkflows int `json:"epic_workflows"`
	Projects      int `json:"projects"`
	Milestones    int `json:"milestones"`
	Iterations    int `json:"iterations"`
	Epics         int `json:"epics"`
	EpicComments  int `json:"epic_comments"`
	Stories       int `json:"stories"`
	Comments      int `json:"comments"`
}

// bump counts one consumed dump line of the given type. The type names
// mirror the dump's line types; unknown names (meta/end) fall through.
func (c *DumpCounts) bump(typ string) {
	switch typ {
	case "members":
		c.Members++
	case "teams":
		c.Teams++
	case "workflows":
		c.Workflows++
	case "epic_workflows":
		c.EpicWorkflows++
	case "projects":
		c.Projects++
	case "milestones":
		c.Milestones++
	case "iterations":
		c.Iterations++
	case "epics":
		c.Epics++
	case "epic_comments":
		c.EpicComments++
	case "stories":
		c.Stories++
	case "comments":
		c.Comments++
	}
}

// Dump is the resumable dump cursor (KV key shortcut:dump). Entity payloads
// stream into KV-backed pieces under shortcut:dump:piece:<n>; the NDJSON
// file is the ordered concatenation, so piece boundaries may fall anywhere
// including mid-story.
type Dump struct {
	Phase      string `json:"phase"`
	StartedAt  string `json:"started_at"` // RFC3339 UTC; names the file
	FinishedAt string `json:"finished_at,omitempty"`
	Filename   string `json:"filename,omitempty"`
	Origin     string `json:"origin,omitempty"` // story-window scan start
	// Skip is the next unprocessed element of the current streaming phase
	// (list endpoint element index, story index within the frozen window,
	// or iteration id index).
	Skip int `json:"skip,omitempty"`
	// Story windows tile [Origin, now) without overlap: a full dump covers
	// every updated_at exactly once. The scan runs newest-first so live
	// progress shows real data immediately; empty gap years (span-doubled)
	// come last.
	WindowStart string `json:"window_start,omitempty"`
	// AtOrigin arms the stories-pass finish: the backwards scan advanced
	// its cursor to or below dumpOrigin, so the next loop-top closes the
	// dump (the window that crossed holds nothing below the origin).
	AtOrigin      bool   `json:"at_origin,omitempty"`
	V             int    `json:"v,omitempty"` // cursor schema; 2 = newest-first stories scan
	WindowStories string `json:"window_stories,omitempty"` // frozen metas
	// IterationIDs dedups iteration bodies inside the stories pass:
	// freezeWindow fetches and emits each body once, before its window's
	// stories, so restore resolves story→sprint links at upsert time.
	IterationIDs []int64 `json:"iteration_ids,omitempty"`
	// SeenMembers dedups per-owner member fetches in the stories pass:
	// each story's unseen assignees are fetched and emitted just before
	// their story line, so restore resolves story→user links at upsert
	// time without ever listing the (unbounded) full member roster.
	SeenMembers map[string]bool `json:"seen_members,omitempty"`
	// WindowSpan is the current stories-pass search span in seconds; 0
	// means the base 24h step. Empty windows double it (capped) so gap
	// years between dumpOrigin and an org's first story cost dozens of
	// ticks instead of thousands; the next frozen window restores the
	// step.
	WindowSpan int64 `json:"window_span,omitempty"`
	// Buf is the open piece; flushed to KV at pieceMax bytes.
	Pieces int        `json:"pieces,omitempty"` // pieces fully written
	Buf    string     `json:"buf,omitempty"`    // open piece body
	Counts DumpCounts `json:"counts"`
	Error  string     `json:"error,omitempty"`
}

// Restore phases.
const (
	restorePhaseStream = "stream"
	restorePhaseDone   = "done"
)

// Restore is the resumable restore cursor (KV key shortcut:restore). The
// CLI posts dump file chunks; each chunk is re-posted until its lines are
// fully consumed, so ChunkIdx resumes exactly where the budget ran out.
type Restore struct {
	Phase      string `json:"phase"`
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	Filename   string `json:"filename,omitempty"` // from the dump's meta line
	// Seq is the chunk sequence being consumed; ChunkIdx is the next line
	// within it. The CLI re-posts chunk seq until chunk_done, then seq+1.
	Seq      int `json:"seq,omitempty"`
	ChunkIdx int `json:"chunk_idx,omitempty"`
	// Carry-over across chunk boundaries: the last upserted entity and its
	// mapped windshift item, so comment lines at the start of the next
	// chunk attach to the right item without a re-lookup.
	CurrentStoryID int64  `json:"current_story_id,omitempty"`
	CurrentEpicID  int64  `json:"current_epic_id,omitempty"`
	CurrentItemID  string `json:"current_item_id,omitempty"`
	// Catalog snapshot accumulated from dump lines (applied before the
	// first story resolves workspaces and parents).
	Workflows     []workflow          `json:"workflows,omitempty"`
	EpicWorkflows []workflow          `json:"epic_workflows,omitempty"`
	Projects      []project           `json:"projects,omitempty"`
	Teams         []team              `json:"teams,omitempty"`
	Members       []shortcutMember    `json:"members,omitempty"`
	Iterations    []shortcutIteration `json:"iterations,omitempty"` // buffered for story-time resolution
	MapsReady     bool                `json:"maps_ready,omitempty"`
	// Counts mirrors the shared engine counters during the run.
	Counts     Counts     `json:"counts"`
	TypeCounts DumpCounts `json:"type_counts,omitempty"`
	LastErrors []string   `json:"last_errors,omitempty"`
	Error      string     `json:"error,omitempty"`
}

func loadDump() (*Dump, error) {
	raw, ok, err := kvGetString(kvDumpKey)
	if err != nil || !ok || raw == "" {
		return nil, err
	}
	var d Dump
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		return nil, fmt.Errorf("dump: %w", err)
	}
	if d.Phase == "members" {
		// Pre-rosterless states: the old full-list /members phase never
		// emitted a line (the list fetch exceeded the budget before Skip
		// advanced), so resuming at teams loses nothing; members now emit
		// inline during the stories pass.
		d.Phase = dumpPhaseTeams
	}
	if d.Phase == dumpPhaseStories && d.V < 2 {
		// Forward-cursor states (pre-newest-first scan): their window
		// fields point at a forward position the reverse scan can't
		// interpret. Restart the stories pass with the new cursor — safe,
		// lines already emitted re-import idempotently (dedup by external
		// id) and the early forward windows were empty anyway on the orgs
		// that hit this.
		d.V = 2
		d.WindowStart = ""
		d.AtOrigin = false
		d.WindowStories = ""
		d.Skip = 0
		d.WindowSpan = 0
	}
	return &d, nil
}

func saveDump(d *Dump) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return kvSetString(kvDumpKey, string(b))
}

func loadRestore() (*Restore, error) {
	raw, ok, err := kvGetString(kvRestoreKey)
	if err != nil || !ok || raw == "" {
		return nil, err
	}
	var r Restore
	if err := json.Unmarshal([]byte(raw), &r); err != nil {
		return nil, fmt.Errorf("restore: %w", err)
	}
	return &r, nil
}

func saveRestore(r *Restore) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return kvSetString(kvRestoreKey, string(b))
}
