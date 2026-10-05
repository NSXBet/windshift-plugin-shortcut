// Sync engine: the resumable tick state machine (contract §6).
//
// One scheduled invocation runs at most ~4.2s of the 5s wasm deadline and
// exits, persisting every cursor in KV. Any deadline abort, 429/5xx, or host
// failure resumes at the same cursor next tick. All upserts are guarded by
// item_lookup + external_updated_at (logic.ShouldSkip), so replays are
// no-ops — cursors are allowed to lag behind reality.
package main

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"shortcut/logic"
)

const (
	tickBudget      = 4200 * time.Millisecond // 5s wasm cap minus state-save margin
	windowStep      = 24 * time.Hour          // fixed frozen window span
	waterOverlap    = 10 * time.Minute        // overlap between consecutive windows
	defaultBackfill = 30 * 24 * time.Hour
	saveEvery       = 25 // upserts processed per mid-window state save
	maxLastErrors   = 10
)

//go:wasmexport sync_tick
func syncTick() {
	runTick()
}

// now is the engine clock. The windshift core pins NOW (RFC3339) in the
// plugin config for deterministic runs; absent → wall clock. The tick budget
// stays on the real clock in both cases: it bounds actual compute time, and
// a pinned clock would freeze its deadline in the past.
func now() time.Time {
	if v := configVar("NOW"); v != "" {
		if t, err := time.Parse(time.RFC3339, v); err == nil {
			return t
		}
	}
	return time.Now()
}

// engine carries one tick's in-memory context. Nothing here survives across
// ticks; all durable progress lives in State (KV).
type engine struct {
	cfg    *Config
	st     *State
	client *shortcutClient
	budget logic.TickBudget
	now    func() time.Time

	stateName     map[int64]string // workflow state id → status name
	epicStateName map[int64]string // epic-workflow state id → status name
	projectName   map[int64]string // project id → project name
}

func (e *engine) run() {
	switch e.st.Phase {
	case phaseCatalog:
		e.runCatalog()
	default:
		e.runStories()
	}
}

// --- catalog phase ---

// runCatalog builds container maps and upserts all epics, then opens the
// first story window. Container lists are refetched per tick (nothing
// in-wasm is persistent); the epic index cursor keeps re-entry cheap.
func (e *engine) runCatalog() {
	if err := e.buildMaps(); err != nil {
		return // transport-level: abort, state untouched, resume next tick
	}
	if err := e.syncEpics(); err != nil {
		return
	}
	e.openWindow(e.now().Add(-e.backfill()))
}

func (e *engine) buildMaps() error {
	wfs, err := e.client.listWorkflows()
	if err != nil {
		return err
	}
	ewfs, err := e.client.listEpicWorkflows()
	if err != nil {
		return err
	}
	projects, err := e.client.listProjects()
	if err != nil {
		return err
	}
	e.stateName = make(map[int64]string, 64)
	for _, wf := range wfs {
		for _, st := range wf.States {
			e.stateName[st.ID] = st.Name
		}
	}
	e.epicStateName = make(map[int64]string, 16)
	for _, wf := range ewfs {
		for _, st := range wf.States {
			e.epicStateName[st.ID] = st.Name
		}
	}
	e.projectName = make(map[int64]string, 64)
	for _, p := range projects {
		e.projectName[p.ID] = p.Name
	}
	return nil
}

func (e *engine) syncEpics() error {
	epics, err := e.client.listEpics()
	if err != nil {
		return err
	}
	for i := e.st.CatalogEpicIdx; i < len(epics); i++ {
		if !e.budget.Remaining() {
			return nil
		}
		ep := epics[i]
		if e.tombstoned("epic", ep.ID) {
			e.st.Counts.Skipped++
			e.st.CatalogEpicIdx = i + 1
			continue
		}
		if err := e.upsertEpic(ep); err != nil {
			if isAborted(err) {
				return err
			}
			e.recordError(err)
		}
		e.st.CatalogEpicIdx = i + 1
	}
	return nil
}

func (e *engine) upsertEpic(ep epic) error {
	if e.cfg.DryRun {
		e.st.Counts.Created++
		logInfo("dry-run: epic " + strconv.FormatInt(ep.ID, 10) + " would upsert")
		return nil
	}
	lk, err := itemLookup("epic", ep.ID)
	if err != nil {
		return err
	}
	skip, err := logic.ShouldSkip(lk.Found, lk.LastSyncedAt, ep.UpdatedAt)
	if err != nil {
		return err
	}
	if skip {
		e.st.Counts.Skipped++
		return nil
	}
	resp, err := itemUpsert(e.epicRequest(ep))
	if err != nil {
		return err
	}
	e.countUpsert(resp)
	return nil
}

func (e *engine) epicRequest(ep epic) itemUpsertRequest {
	req := itemUpsertRequest{
		ExternalKind:      "epic",
		ExternalID:        ep.ID,
		ExternalURL:       ep.AppURL,
		ExternalUpdatedAt: ep.UpdatedAt,
		WorkspaceID:       e.cfg.WorkspaceID,
		Title:             ep.Name,
		Description:       ep.Description,
		StatusName:        e.epicStateName[ep.EpicStateID],
		ItemTypeName:      "Epic",
		Labels:            labelRefs(ep.Labels),
		LabelMode:         e.cfg.LabelMode,
	}
	if ep.Deadline != nil && len(*ep.Deadline) >= 10 {
		req.DueDate = (*ep.Deadline)[:10]
	}
	return req
}

// --- stories phase ---

func (e *engine) runStories() {
	if err := e.buildMaps(); err != nil {
		return
	}
	if e.st.StoryWindowStart == "" {
		e.openWindow(e.watermark())
	}
	stories, err := e.searchWindow()
	if err != nil {
		return
	}
	if e.st.StoryIdx > len(stories) {
		// The array shifted under frozen bounds (workspace retention edge).
		// Restart the window from zero — replays are guarded no-ops.
		e.st.StoryIdx = 0
	}
	e.processStories(stories)
}

// runTick is the whole engine one-shot, shared by the 5m schedule (the
// sync_tick wasm export above) and the admin "Tick now" route (tk-swm).
func runTick() {
	cfg, err := loadConfig()
	if err != nil {
		logInfo("sync_tick: config load failed: " + truncateSnippet(err.Error(), 300))
		return
	}
	if cfg == nil {
		// Config key absent: not provisioned yet — stay silent until the
		// operator form writes a config (loadConfig returns nil, nil).
		return
	}
	if !cfg.Enabled {
		return
	}
	st, err := loadState()
	if err != nil {
		logInfo("sync_tick: state corrupt (operator reset required): " + truncateSnippet(err.Error(), 300))
		return
	}
	if st == nil {
		// Fresh install: state key absent → start from the catalog pass.
		st = &State{Phase: phaseCatalog}
	}
	e := &engine{
		cfg:    cfg,
		st:     st,
		client: newShortcutClient(cfg),
		budget: logic.NewTickBudget(time.Now(), tickBudget),
		now:    now,
	}
	defer e.save()
	e.run()
}

// runSweep is the end-of-window deletion fallback (tk-thf): Shortcut
// hard-deletes leave no API trace, so every mapped story id seen in this
// window is re-verified with a canonical GET — a 404 now means the story
// died after the window froze, and the tombstone path runs (source: sweep).
// Resumable at StorySweepIdx; one tick sweeps as much as the budget allows.
func (e *engine) runSweep(stories []story) bool {
	for i := e.st.StorySweepIdx; i < len(stories); i++ {
		if !e.budget.Remaining() {
			return false
		}
		s := stories[i]
		e.st.StorySweepIdx = i + 1
		if e.tombstoned("story", s.ID) {
			continue
		}
		lk, err := itemLookup("story", s.ID)
		switch {
		case isAborted(err):
			e.st.StorySweepIdx = i
			return false
		case err != nil:
			e.recordError(err)
			continue
		case !lk.Found:
			continue // never synced → nothing in Windshift to verify
		}
		_, err = e.client.getStory(s.ID)
		switch {
		case isNotFound(err):
			if err := tombstoneStory(e.cfg, s.ID, "sweep"); err != nil {
				e.recordError(err)
			}
		case isAborted(err):
			e.st.StorySweepIdx = i
			return false
		case err != nil:
			e.recordError(err)
		}
	}
	return true
}

// searchWindow fetches the frozen window's array. Windows are fixed bounds
// (no pagination on stories/search, contract §4), so the array is stable
// across ticks and StoryIdx resume is valid.
func (e *engine) searchWindow() ([]story, error) {
	start, err := time.Parse(time.RFC3339, e.st.StoryWindowStart)
	if err != nil {
		logInfo("sync_tick: window start corrupt: " + truncateSnippet(err.Error(), 300))
		return nil, err
	}
	end, err := time.Parse(time.RFC3339, e.st.StoryWindowEnd)
	if err != nil {
		logInfo("sync_tick: window end corrupt: " + truncateSnippet(err.Error(), 300))
		return nil, err
	}
	return e.client.searchStories(start, end)
}

// processStories advances through the fetched array. The cursor is set to
// the NEXT unprocessed index before each item: a crash after a save replays
// at most saveEvery items, all of which re-check item_lookup first.
func (e *engine) processStories(stories []story) {
	sinceSave := 0
	for i := e.st.StoryIdx; i < len(stories); i++ {
		if !e.budget.Remaining() {
			return
		}
		s := stories[i]
		e.st.StoryIdx = i + 1
		if e.tombstoned("story", s.ID) {
			e.st.Counts.Skipped++
		} else if err := e.upsertStory(s); err != nil {
			if isAborted(err) {
				return
			}
			e.recordError(err)
		}
		sinceSave++
		if sinceSave == saveEvery {
			sinceSave = 0
			e.save()
		}
	}
	// Story array exhausted → comment pass over the same frozen window
	// (contract §6). The window parks only after the comment cursor also
	// reaches the end, so an aborted comment pass resumes mid-window.
	if incomplete, err := e.runComments(stories); err != nil {
		return
	} else if incomplete {
		e.save()
		logInfo("comment pass incomplete: " + e.countsSummary())
		return
	}
	if done := e.runSweep(stories); !done {
		return
	}
	// Window exhausted → park the cursor; next tick opens a fresh window
	// from the watermark (end - overlap).
	e.st.LastWindowEnd = e.st.StoryWindowEnd
	e.st.StoryWindowStart = ""
	e.st.StoryWindowEnd = ""
	e.st.StoryIdx = 0
	e.st.CommentIdx = 0
	e.st.StorySweepIdx = 0
	e.save()
	logInfo("window done " + e.st.LastWindowEnd + ": " + e.countsSummary())
}

func (e *engine) watermark() time.Time {
	if e.st.LastWindowEnd != "" {
		if end, err := time.Parse(time.RFC3339, e.st.LastWindowEnd); err == nil {
			return logic.AdvanceWatermark(end, waterOverlap)
		}
	}
	return e.now().Add(-e.backfill())
}

func (e *engine) openWindow(wm time.Time) {
	start, end := logic.FreshWindow(wm, e.now(), windowStep)
	e.st.Phase = phaseStories
	e.st.StoryWindowStart = start.UTC().Format(time.RFC3339)
	e.st.StoryWindowEnd = end.UTC().Format(time.RFC3339)
	e.st.StoryIdx = 0
	e.st.CommentIdx = 0
	e.st.StorySweepIdx = 0
}

func (e *engine) backfill() time.Duration {
	if e.cfg.BackfillDays > 0 {
		return time.Duration(e.cfg.BackfillDays) * 24 * time.Hour
	}
	return defaultBackfill
}

func (e *engine) upsertStory(s story) error {
	if e.cfg.DryRun {
		e.st.Counts.Created++
		logInfo("dry-run: story " + strconv.FormatInt(s.ID, 10) + " would upsert")
		return nil
	}
	lk, err := itemLookup("story", s.ID)
	if err != nil {
		return err
	}
	skip, err := logic.ShouldSkip(lk.Found, lk.LastSyncedAt, s.UpdatedAt)
	if err != nil {
		return err
	}
	if skip {
		e.st.Counts.Skipped++
		return nil
	}
	resp, err := itemUpsert(e.storyRequest(s))
	if err != nil {
		return err
	}
	e.countUpsert(resp)
	return nil
}

func (e *engine) storyRequest(s story) itemUpsertRequest {
	req := itemUpsertRequest{
		ExternalKind:      "story",
		ExternalID:        s.ID,
		ExternalURL:       s.AppURL,
		ExternalUpdatedAt: s.UpdatedAt,
		WorkspaceID:       e.cfg.WorkspaceID,
		Title:             s.Name,
		Description:       s.Description,
		StatusName:        e.stateName[s.WorkflowStateID],
		ItemTypeName:      logic.ItemTypeName(s.StoryType),
		ProjectName:       e.projectName[s.ProjectID],
		Labels:            s.labelNames(),
		LabelMode:         e.cfg.LabelMode,
	}
	if s.Estimate != nil {
		req.StoryPoints = *s.Estimate
	}
	if s.Deadline != nil && len(*s.Deadline) >= 10 {
		req.DueDate = (*s.Deadline)[:10]
	}
	if s.EpicID != nil {
		req.ParentExternalKind = "epic"
		req.ParentExternalID = *s.EpicID
	}
	return req
}

// --- comments phase (contract §6; runs inside the stories window) ---

// runComments imports comments for the window's stories from CommentIdx.
// Create-only with a KV dedup map: every live (non-deleted) Shortcut comment
// becomes one windshift comment authored by the configured actor with
// notifications suppressed. Returns (incomplete, nil) when the tick budget
// ran out mid-story, or (_, err) only on a transport abort — the caller then
// rewinds the cursor to the unprocessed story.
func (e *engine) runComments(stories []story) (bool, error) {
	for i := e.st.CommentIdx; i < len(stories); i++ {
		if !e.budget.Remaining() {
			return true, nil
		}
		s := stories[i]
		e.st.CommentIdx = i + 1
		if e.tombstoned("story", s.ID) {
			continue
		}
		incomplete, err := e.importStoryComments(s)
		switch {
		case err != nil && isAborted(err):
			e.st.CommentIdx = i // resume this story's comments next tick
			return false, err
		case err != nil:
			e.recordError(err) // item-level rejection: visible in state, move on
		case incomplete:
			e.st.CommentIdx = i // budget ran out mid-story; refetch its comments
			return true, nil
		}
	}
	return false, nil
}

// importStoryComments attaches one story's comments to its mapped windshift
// item. A story with no mapping (skipped in the story pass, or created after
// this window froze) has nothing to attach to — treated as done.
func (e *engine) importStoryComments(s story) (bool, error) {
	if e.cfg.DryRun {
		cs, err := e.client.listStoryComments(s.ID)
		if err != nil {
			return false, err
		}
		live := 0
		for _, c := range cs {
			if !c.Deleted {
				live++
			}
		}
		e.st.Counts.Comments += live
		logInfo("dry-run: story " + strconv.FormatInt(s.ID, 10) + " has " + strconv.Itoa(live) + " live comments")
		return false, nil
	}
	lk, err := itemLookup("story", s.ID)
	if err != nil {
		return false, err
	}
	if !lk.Found {
		return false, nil
	}
	itemID, err := itemIDInt(lk.ItemID)
	if err != nil {
		return false, err
	}
	cs, err := e.client.listStoryComments(s.ID)
	if err != nil {
		return false, err
	}
	for _, c := range cs {
		if c.Deleted || c.Text == "" {
			continue
		}
		if !e.budget.Remaining() {
			return true, nil
		}
		if err := e.importComment(itemID, c); err != nil {
			if isAborted(err) {
				return false, err
			}
			e.recordError(err)
		}
	}
	return false, nil
}

// importComment creates one windshift comment unless the dedup map already
// holds it. The KV record is written only after create_comment succeeds, so
// a crash in between would duplicate on replay — the map lookup first keeps
// replays no-ops (contract §6).
func (e *engine) importComment(itemID int, c storyComment) error {
	if _, found, err := kvGetString(commentKey(c.ID)); err != nil {
		return err
	} else if found {
		e.st.Counts.Skipped++
		return nil
	}
	resp, err := createComment(createCommentRequest{
		ItemID:                itemID,
		AuthorID:              int(e.cfg.ActorUserID),
		Content:               c.Text,
		SuppressNotifications: true,
	})
	if err != nil {
		return err
	}
	if err := kvSetString(commentKey(c.ID), strconv.Itoa(resp.CommentID)); err != nil {
		return err
	}
	e.st.Counts.Comments++
	return nil
}

// --- shared helpers ---

func labelRefs(ls []labelRef) []string {
	names := make([]string, 0, len(ls))
	for _, l := range ls {
		names = append(names, l.Name)
	}
	return names
}

func (e *engine) countUpsert(resp itemUpsertResponse) {
	if resp.Created {
		e.st.Counts.Created++
	} else {
		e.st.Counts.Updated++
	}
}

// tombstoned reports whether the id carries a deletion marker (contract §5);
// marker lookup failures are treated as not-tombstoned so a bad KV read can
// never silently hide a live story.
func (e *engine) tombstoned(kind string, id int64) bool {
	_, found, err := kvGetString(tombstoneKey(kind, id))
	return err == nil && found
}

// isAborted classifies err as transport-level (rate limit, Shortcut 5xx,
// http_fetch transport failure, host-function failure): the tick aborts and
// resumes next invocation. Item-level API errors (400 schema mismatch etc.)
// are recorded and skipped instead (contract §6).
func isAborted(err error) bool {
	if err == nil {
		return false // a successful call is never a transport failure
	}
	var e *errAPIStatus
	if errors.As(err, &e) {
		return e.Status == 429 || e.Status >= 500
	}
	// Item-level host rejections are recorded and skipped, never aborted —
	// otherwise a core rejection (e.g. a bad epic status_name) retries
	// forever at the same cursor.
	return !isRejected(err)
}

func (e *engine) recordError(err error) {
	e.st.Counts.Errors++
	msg := truncateSnippet(err.Error(), 200)
	e.st.LastErrors = append(e.st.LastErrors, msg)
	if len(e.st.LastErrors) > maxLastErrors {
		e.st.LastErrors = e.st.LastErrors[len(e.st.LastErrors)-maxLastErrors:]
	}
	logInfo("sync error: " + msg)
}

func (e *engine) save() {
	if err := saveState(e.st); err != nil {
		logInfo("state save failed: " + truncateSnippet(err.Error(), 200))
	}
}

func (e *engine) countsSummary() string {
	c := e.st.Counts
	return fmt.Sprintf("created=%d updated=%d skipped=%d errors=%d comments=%d", c.Created, c.Updated, c.Skipped, c.Errors, c.Comments)
}
