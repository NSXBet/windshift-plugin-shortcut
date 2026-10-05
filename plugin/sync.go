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
	cfg, err := loadConfig()
	if err != nil {
		logInfo("sync_tick: config load failed: " + truncateSnippet(err.Error(), 300))
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
	e := &engine{
		cfg:    cfg,
		st:     st,
		client: newShortcutClient(cfg),
		budget: logic.NewTickBudget(time.Now(), tickBudget),
		now:    time.Now,
	}
	e.run()
	if err := saveState(e.st); err != nil {
		logInfo("sync_tick: state save failed: " + truncateSnippet(err.Error(), 300))
	}
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
	// Window exhausted → park the cursor; next tick opens a fresh window
	// from the watermark (end - overlap).
	e.st.LastWindowEnd = e.st.StoryWindowEnd
	e.st.StoryWindowStart = ""
	e.st.StoryWindowEnd = ""
	e.st.StoryIdx = 0
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
	var e *errAPIStatus
	if errors.As(err, &e) {
		return e.Status == 429 || e.Status >= 500
	}
	return true
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
	return fmt.Sprintf("created=%d updated=%d skipped=%d errors=%d", c.Created, c.Updated, c.Skipped, c.Errors)
}
