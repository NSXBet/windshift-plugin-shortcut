// Sync engine: the resumable tick state machine (contract §6).
//
// One scheduled invocation runs at most ~4.2s of the 5s wasm deadline and
// exits, persisting every cursor in KV. Any deadline abort, 429/5xx, or host
// failure resumes at the same cursor next tick. All upserts are guarded by
// item_lookup + external_updated_at (logic.ShouldSkip), so replays are
// no-ops — cursors are allowed to lag behind reality.
package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
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

// now is the engine clock. A NOW (RFC3339) value in the plugin's extism
// manifest config pins it — the test harness uses that for deterministic
// runs; the live core sets no config, so the wall clock applies. Before the
// core passed WithSysWalltime, wazero's fake clock (2022-01-01) surfaced
// here too and livelocked the watermark — see core manager.go
// pluginInstanceConfig.

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

	stateType     map[int64]string // workflow state id → state type (backlog/unstarted/started/done)
	stateName     map[int64]string // workflow state id → state name (the board column a story lands in)
	epicStateType map[int64]string // epic-workflow state id → state type
	projectName   map[int64]string // project id → project name
	teamByState   map[int64]int64  // workflow state id → Shortcut team id (team mode placement)
	teams         map[int64]*team  // Shortcut team id → team (name + states, from /teams)
	wsByProject   map[int64]string // project id → resolved windshift workspace id (per tick cache)
	wsByTeam      map[int64]string // team id → resolved windshift workspace id (per tick cache)
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
func (e *engine) runCatalog() {
	logInfo("runCatalog enter, budget left ms=" + strconv.FormatInt(e.budget.DeadlineMs(), 10))
	if err := e.buildMaps(); err != nil {
		e.recordError(err) // transport-level: abort, resume next tick; surfaced in state
		return
	}
	if e.cfg.WorkspaceMode == "team" {
		if err := e.syncTeams(); err != nil {
			e.recordError(err) // transport-level: abort, resume next tick
			return
		}
	}
	paused, err := e.condenseEpics()
	if err != nil {
		return
	}
	if paused {
		return
	}
	paused, err = e.syncEpics()
	if err != nil {
		return
	}
	if paused {
		// Epic pass incomplete: do not open the story window on a dead
		// budget — the next fetch would outlive the wasm cap and kill the
		// module before the state save. Resume at the cursor next tick.
		return
	}
	e.openWindow(e.now().Add(-e.backfill()))
}

// condenseEpics freezes the /epics stream into state before the catalog
// runs. The ~4 MB payload re-fetched on every tick starved the budget —
// fetch ~2.5s plus full-struct decode left single-digit epics of headroom,
// and a killed module loses the tick's progress — so one pass collects as
// many (id, updated_at) metas as the budget allows and the next tick
// resumes at the cursor. The pass that drains the stream flags the catalog
// condensed; every later reset re-freezes from scratch (fresh bodies are
// needed only for epics whose upsert runs).
func (e *engine) condenseEpics() (paused bool, err error) {
	if e.st.CatalogCondensed {
		return false, nil
	}
	var metas []epicMeta
	if e.st.CatalogEpics != "" {
		if err := json.Unmarshal([]byte(e.st.CatalogEpics), &metas); err != nil {
			// Corrupt chunk: restart the condense from zero.
			metas = nil
			e.st.CatalogEpicIdx = 0
		}
	}
	err = e.client.collectEpicMetas(e.st.CatalogEpicIdx, func(id int64, updatedAt string) error {
		if e.budget.DeadlineMs() < 800 {
			// Keep room for the chunk save; a wasm cap kill here would
			// discard the whole pass.
			return errBudgetPause
		}
		metas = append(metas, epicMeta{ID: id, UpdatedAt: updatedAt})
		e.st.CatalogEpicIdx++
		return nil
	})
	if errors.Is(err, errBudgetPause) {
		e.saveCatalogEpics(metas)
		return true, nil
	}
	if err != nil {
		return false, err
	}
	e.saveCatalogEpics(metas)
	e.st.CatalogCondensed = true
	logInfo("epic catalog condensed: " + strconv.Itoa(len(metas)) + " epics")
	return false, nil
}

// saveCatalogEpics persists the condensed metas chunk. Each chunk save is
// ~100 bytes per epic — cheap next to the 4 MB stream that used to be
// re-fetched per tick.
func (e *engine) saveCatalogEpics(metas []epicMeta) {
	if raw, err := json.Marshal(metas); err == nil {
		e.st.CatalogEpics = string(raw)
	}
	e.save()
}

func (e *engine) syncEpics() (paused bool, err error) {
	var metas []epicMeta
	if err := json.Unmarshal([]byte(e.st.CatalogEpics), &metas); err != nil {
		// The frozen catalog is corrupt (partial write): rebuild it from
		// zero on the next tick.
		e.st.CatalogEpics = ""
		e.st.CatalogCondensed = false
		e.st.CatalogEpicIdx = 0
		return true, nil
	}
	for i := e.st.CatalogEpicIdx; i < len(metas); i++ {
		if !e.budget.Remaining() {
			return true, nil
		}
		m := metas[i]
		e.st.CatalogEpicIdx = i + 1
		if e.tombstoned("epic", m.ID) {
			e.st.Counts.Skipped++
		} else if err := e.upsertEpicMeta(m); err != nil {
			if isAborted(err) {
				// Transport abort: rewind so this epic retries next tick.
				e.st.CatalogEpicIdx = i
				return false, err
			}
			e.recordError(err)
		}
		// Checkpoint the cursor EVERY epic: a wasm invocation killed at
		// the 5s cap never reaches the end-of-tick save, so without this
		// the next tick replays the same prefix until it is killed again.
		// One kv_set per epic is cheap next to the per-epic kv_get.
		e.save()
	}
	return false, nil
}

// upsertEpicMeta re-verifies one frozen epic: lookup skip, lazy body fetch
// only for epics that are new or changed since their last sync.
func (e *engine) upsertEpicMeta(m epicMeta) error {
	if e.cfg.DryRun {
		e.st.Counts.Created++
		logInfo("dry-run: epic " + strconv.FormatInt(m.ID, 10) + " would upsert")
		return nil
	}
	lk, err := itemLookup("epic", m.ID)
	if err != nil {
		return err
	}
	skip, err := logic.ShouldSkip(lk.Found, lk.LastSyncedAt, m.UpdatedAt)
	if err != nil {
		return err
	}
	if skip {
		e.st.Counts.Skipped++
		return nil
	}
	ep, err := e.client.getEpic(m.ID)
	if err != nil {
		return fmt.Errorf("epic %d: %w", m.ID, err)
	}
	// Epics are org-level (all 2484 carry no team in this org): they always
	// land in the configured workspace, regardless of mode.
	resp, err := itemUpsert(e.epicRequest(*ep, e.cfg.WorkspaceID))
	if err != nil {
		return err
	}
	e.countUpsert(resp)
	return nil
}

func (e *engine) buildMaps() error {
	if e.st.MapsJSON != "" {
		var m struct {
			StateType     map[int64]string   `json:"st"`
			StateName     map[int64]string   `json:"sn"`
			EpicStateType map[int64]string   `json:"et"`
			ProjectName   map[int64]string   `json:"pn"`
			TeamByState   map[int64]int64    `json:"ts"`
			Teams         map[int64]teamMeta `json:"tt"`
		}
		// Team mode additionally requires the cached team roster: a cache
		// written by an older build (or another mode) lacks it.
		teamsOK := e.cfg.WorkspaceMode != "team" || m.Teams != nil
		if err := json.Unmarshal([]byte(e.st.MapsJSON), &m); err == nil &&
			m.StateType != nil && m.StateName != nil && m.EpicStateType != nil && m.ProjectName != nil && teamsOK {
			e.stateType = m.StateType
			e.stateName = m.StateName
			e.epicStateType = m.EpicStateType
			e.projectName = m.ProjectName
			e.teamByState = m.TeamByState
			if e.cfg.WorkspaceMode == "per_project" {
				e.wsByProject = make(map[int64]string, len(m.ProjectName))
			}
			if e.cfg.WorkspaceMode == "team" {
				e.teams = make(map[int64]*team, len(m.Teams))
				for id, tm := range m.Teams {
					e.teams[id] = &team{Name: tm.Name, States: tm.States}
				}
				e.wsByTeam = make(map[int64]string, len(m.Teams))
			}
			return nil
		}
		// Corrupt cache: fall through to a fresh fetch.
	}
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
	e.stateType = make(map[int64]string, 64)
	e.stateName = make(map[int64]string, 64)
	e.teamByState = make(map[int64]int64, 64)
	for _, wf := range wfs {
		for _, st := range wf.States {
			e.stateType[st.ID] = st.Type
			e.stateName[st.ID] = st.Name
			if wf.TeamID != nil {
				// A story's team is its workflow's team: state id → team id.
				e.teamByState[st.ID] = *wf.TeamID
			}
		}
	}
	e.epicStateType = make(map[int64]string, 16)
	for _, wf := range ewfs {
		for _, st := range wf.States {
			e.epicStateType[st.ID] = st.Type
		}
	}
	e.projectName = make(map[int64]string, 64)
	for _, p := range projects {
		e.projectName[p.ID] = p.Name
	}
	if e.cfg.WorkspaceMode == "team" {
		if err := e.loadTeams(); err != nil {
			return err
		}
	}
	if raw, err := json.Marshal(struct {
		StateType     map[int64]string   `json:"st"`
		StateName     map[int64]string   `json:"sn"`
		EpicStateType map[int64]string   `json:"et"`
		ProjectName   map[int64]string   `json:"pn"`
		TeamByState   map[int64]int64    `json:"ts"`
		Teams         map[int64]teamMeta `json:"tt"`
	}{e.stateType, e.stateName, e.epicStateType, e.projectName, e.teamByState, e.teamMetas()}); err == nil {
		e.st.MapsJSON = string(raw)
	}
	if e.cfg.WorkspaceMode == "per_project" {
		e.wsByProject = make(map[int64]string, len(projects))
	}
	if e.cfg.WorkspaceMode == "team" {
		e.wsByTeam = make(map[int64]string, len(e.teams))
	}
	return nil
}

// loadTeams fetches the team roster (GET /teams, each embedded workflow
// carries the states the workspace board will bind). Small org payload —
// one fetch, kept in the maps cache so story ticks never refetch it.
func (e *engine) loadTeams() error {
	teams, err := e.client.listTeams()
	if err != nil {
		return err
	}
	e.teams = make(map[int64]*team, len(teams))
	for i := range teams {
		t := teams[i]
		if t.Workflow != nil {
			t.States = t.Workflow.States
		}
		e.teams[t.ID] = &t
	}
	return nil
}

// teamMeta is the maps-cache form of a team (states included — the workspace
// board needs them and the roster is tiny).
type teamMeta struct {
	Name   string          `json:"n"`
	States []workflowState `json:"s,omitempty"`
}

func (e *engine) teamMetas() map[int64]teamMeta {
	if e.teams == nil {
		return nil
	}
	out := make(map[int64]teamMeta, len(e.teams))
	for id, t := range e.teams {
		out[id] = teamMeta{Name: t.Name, States: t.States}
	}
	return out
}

// resolveWorkspace returns the windshift workspace id a story should target.
// single mode: the configured workspace. per_project: the story's project
// gets its own workspace via workspace_upsert (find-or-create by project id,
// cached per tick); projects missing from /projects fall back to the
// configured workspace, which config.validate requires in every mode. team
// mode: the story's workflow state → its workflow's Shortcut team.
func (e *engine) resolveWorkspace(s story) (string, error) {
	switch e.cfg.WorkspaceMode {
	case "team":
		return e.resolveTeamWorkspace(e.teamForStory(s.WorkflowStateID))
	case "per_project":
		// fall through to project resolution below
	default:
		// "single" (the empty string predates the mode field)
		return e.cfg.WorkspaceID, nil
	}
	projectID := s.ProjectID
	if ws, ok := e.wsByProject[projectID]; ok {
		return ws, nil
	}
	name, known := e.projectName[projectID]
	if !known || name == "" {
		return e.cfg.WorkspaceID, nil
	}
	resp, err := workspaceUpsert(workspaceUpsertRequest{
		ExternalID:   projectID,
		ExternalKind: "project",
		Name:         name,
	})
	if err != nil {
		return "", err
	}
	e.wsByProject[projectID] = resp.WorkspaceID
	return resp.WorkspaceID, nil
}

// storyStatusName resolves the status a story should carry: the workflow
// state's own name (the team board's column) when known, else the
// category-style seed name derived from the state type.
func (e *engine) storyStatusName(stateID int64) string {
	if name, ok := e.stateName[stateID]; ok && name != "" {
		return name
	}
	return logic.StatusNameForStateType(e.stateType[stateID])
}

// teamForStory maps a story's workflow state to its Shortcut team. The
// workflow owns the team relationship (workflow.team_id), so placement
// follows the state the story sits in. Unknown states (0) fall back to the
// configured workspace at the resolveTeamWorkspace level.
func (e *engine) teamForStory(stateID int64) int64 {
	if teamID, ok := e.teamByState[stateID]; ok {
		return teamID
	}
	return 0
}

// resolveTeamWorkspace returns the windshift workspace for a Shortcut team
// id via workspace_upsert (find-or-create, per-tick cached). teamID 0 or an
// unknown team falls back to the configured workspace.
func (e *engine) resolveTeamWorkspace(teamID int64) (string, error) {
	if teamID == 0 {
		return e.cfg.WorkspaceID, nil
	}
	if ws, ok := e.wsByTeam[teamID]; ok {
		return ws, nil
	}
	t, known := e.teams[teamID]
	if !known || t == nil || t.Name == "" {
		return e.cfg.WorkspaceID, nil
	}
	req := workspaceUpsertRequest{
		ExternalID:   teamID,
		ExternalKind: "team",
		Name:         t.Name,
	}
	for _, st := range t.States {
		req.States = append(req.States, workspaceUpsertState{Name: st.Name, Type: st.Type})
	}
	resp, err := workspaceUpsert(req)
	if err != nil {
		return "", err
	}
	e.wsByTeam[teamID] = resp.WorkspaceID
	return resp.WorkspaceID, nil
}

// syncTeams creates every Shortcut team's workspace with its workflow bound
// (workspace_upsert carries the states). Idempotent and tiny (one digit
// count of teams), so no cursor: a budget pause replays the whole roster
// next tick and already-created teams no-op inside workspace_upsert.
// Item-level rejections are recorded and the roster keeps going.
func (e *engine) syncTeams() error {
	ids := make([]int64, 0, len(e.teams))
	for id := range e.teams {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, teamID := range ids {
		t := e.teams[teamID]
		if !e.budget.Remaining() {
			return errBudgetPause
		}
		if _, err := e.resolveTeamWorkspace(teamID); err != nil {
			if isAborted(err) {
				return err
			}
			e.recordError(fmt.Errorf("team workspace %s: %w", t.Name, err))
		}
	}
	return nil
}

func (e *engine) epicRequest(ep epic, wsID string) itemUpsertRequest {
	title, description := logic.TitleAndDescription(ep.Name, ep.Description)
	req := itemUpsertRequest{
		ExternalKind:      "epic",
		ExternalID:        ep.ID,
		ExternalURL:       ep.AppURL,
		ExternalUpdatedAt: ep.UpdatedAt,
		WorkspaceID:       wsID,
		Title:             title,
		Description:       description,
		StatusName:        logic.StatusNameForStateType(e.epicStateType[ep.EpicStateID]),
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
		// A silent return here reads as a frozen engine: counts never move
		// and last_errors keeps showing stale entries. Record it.
		e.recordError(err)
		return
	}
	if e.st.StoryWindowStart == "" {
		// Stage 1: open the window and SAVE before any network work — with
		// the window persisted, a killed module (5s wasm cap) resumes here
		// instead of replaying the advance forever.
		e.openWindow(e.watermark())
		e.save()
	}
	if e.st.WindowStories == "" {
		// Stage 2: search and freeze the result. One 3.5s fetch; the
		// condensed list is saved immediately so the pass below never
		// re-searches after a resume.
		stories, err := e.searchWindow()
		if err != nil {
			e.recordError(err)
			return
		}
		e.st.WindowStories = string(condenseStories(stories))
		e.save()
	}
	metas := e.decodeWindowStories()
	if e.st.StoryIdx > len(metas) {
		// The array shifted under frozen bounds (workspace retention edge).
		// Restart the window from zero — replays are guarded no-ops.
		e.st.StoryIdx = 0
	}
	e.processStories(metas)
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
	e.client.remainingMs = func() int64 { return e.budget.DeadlineMs() }
	defer e.save()
	e.run()
}

// runSweep is the end-of-window deletion fallback (tk-thf): Shortcut
// hard-deletes leave no API trace, so every mapped story id seen in this
// window is re-verified with a canonical GET — a 404 now means the story
// died after the window froze, and the tombstone path runs (source: sweep).
// Resumable at StorySweepIdx; one tick sweeps as much as the budget allows.
//
// Initial-import mode (backfill_days > 365): every story this window covered
// was created or canonically re-verified by the story pass moments earlier —
// sweeping it again doubles the API cost for zero new information. The
// operator drops backfill_days to the operational value after the migration
// and sweeps resume on the rolling windows.
func (e *engine) runSweep(metas []storyMeta) bool {
	if e.cfg.BackfillDays > 365 {
		return true
	}
	for i := e.st.StorySweepIdx; i < len(metas); i++ {
		if !e.budget.Remaining() {
			return false
		}
		m := metas[i]
		e.st.StorySweepIdx = i + 1
		if e.tombstoned("story", m.ID) {
			continue
		}
		lk, err := itemLookup("story", m.ID)
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
		_, err = e.client.getStory(m.ID)
		switch {
		case isNotFound(err):
			if err := tombstoneStory(e.cfg, m.ID, "sweep"); err != nil {
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

// condenseStories freezes a window's search result: only the fields the
// passes need (id + update stamp). Bodies are fetched lazily by syncStory so
// a skip-heavy replay never re-transfers the window. The condensed JSON also
// makes the frozen window fit the KV state even for dense days.
func condenseStories(stories []story) []byte {
	metas := make([]storyMeta, len(stories))
	for i, s := range stories {
		metas[i] = storyMeta{ID: s.ID, UpdatedAt: s.UpdatedAt}
	}
	raw, err := json.Marshal(metas)
	if err != nil {
		// []storyMeta is plain ints/strings; marshal cannot fail here.
		panic("condenseStories: " + err.Error())
	}
	return raw
}

// decodeWindowStories restores the frozen window. Corrupt JSON parks the
// window (empty metas) — processStories finishes it immediately and the next
// window re-searches; nothing is imported with wrong identities.
func (e *engine) decodeWindowStories() []storyMeta {
	var metas []storyMeta
	if err := json.Unmarshal([]byte(e.st.WindowStories), &metas); err != nil {
		logInfo("sync_tick: window stories corrupt: " + truncateSnippet(err.Error(), 300))
		e.recordError(fmt.Errorf("window stories corrupt: %w", err))
		return nil
	}
	return metas
}

// processStories advances through the frozen window. The cursor is set to
// the NEXT unprocessed index before each item: a crash after a save replays
// at most saveEvery items, all of which re-check item_lookup first. Story
// bodies are fetched lazily — skip-only ticks stay at one host call each.
func (e *engine) processStories(metas []storyMeta) {
	sinceSave := 0
	for i := e.st.StoryIdx; i < len(metas); i++ {
		if !e.budget.Remaining() {
			return
		}
		m := metas[i]
		e.st.StoryIdx = i + 1
		if e.tombstoned("story", m.ID) {
			e.st.Counts.Skipped++
		} else if err := e.syncStory(m); err != nil {
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
	if incomplete, err := e.runComments(metas); err != nil {
		return
	} else if incomplete {
		e.save()
		logInfo("comment pass incomplete: " + e.countsSummary())
		return
	}
	if done := e.runSweep(metas); !done {
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
	e.st.WindowStories = ""
	e.save()
	logInfo("window done " + e.st.LastWindowEnd + ": " + e.countsSummary())
}

// syncStory upserts one window story. The body fetch happens only after the
// skip decision — the window slice is id/date-stamped only. A story deleted
// after the window froze (404) is recorded as skipped, never an error; the
// deletion sweep is the authoritative detector.
func (e *engine) syncStory(m storyMeta) error {
	lk, err := itemLookup("story", m.ID)
	if err != nil {
		return err
	}
	skip, err := logic.ShouldSkip(lk.Found, lk.LastSyncedAt, m.UpdatedAt)
	if err != nil {
		return err
	}
	if skip {
		e.st.Counts.Skipped++
		return nil
	}
	s, err := e.client.getStory(m.ID)
	if err != nil {
		if isNotFound(err) {
			e.st.Counts.Skipped++
			return nil
		}
		return err
	}
	return e.upsertStory(*s)
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
	wsID, err := e.resolveWorkspace(s)
	if err != nil {
		return err
	}
	req := e.storyRequest(s, wsID)
	if s.EpicID != nil {
		parentable, err := e.ensureParentEpic(*s.EpicID)
		if err != nil {
			if !isNotFound(err) {
				return err
			}
			// Parent epic deleted in Shortcut (children outlive it): the
			// mapping can never appear, so import the child parentless
			// instead of failing it at the same cursor forever.
			parentable = false
		}
		if !parentable {
			req.ParentExternalKind, req.ParentExternalID = "", 0
		}
	}
	if len(s.OwnerIDs) > 0 {
		if err := e.ensureMember(s.OwnerIDs[0]); err != nil {
			return err
		}
		req.AssigneeExternalID = s.OwnerIDs[0]
	}
	if s.IterationID != nil && *s.IterationID > 0 {
		if err := e.ensureIteration(*s.IterationID, wsID); err != nil {
			return err
		}
		req.IterationExternalID = *s.IterationID
	}
	resp, err := itemUpsert(req)
	if err != nil {
		return err
	}
	e.countUpsert(resp)
	return nil
}

// ensureMember imports a story's assignee on demand (member_upsert). The
// core maps member id → user, so stories can carry a real owner without a
// member roster pass; item-level rejections bubble up as rejections.
func (e *engine) ensureMember(memberID string) error {
	m, err := e.client.getMember(memberID)
	if err != nil {
		if isNotFound(err) {
			// Member deleted in Shortcut: drop the assignment rather than
			// failing the story.
			return nil
		}
		return fmt.Errorf("assignee %s: %w", memberID, err)
	}
	_, err = memberUpsert(memberUpsertRequest{
		ExternalID: m.ID,
		Name:       m.Name,
		Email:      m.Email,
		Username:   m.Username,
	})
	return err
}

// ensureIteration imports a story's sprint on demand, scoped to the story's
// workspace (windshift iterations are workspace-local). A missing iteration
// in Shortcut (404) drops the reference the same way epics do.
func (e *engine) ensureIteration(iterationID int64, wsID string) error {
	it, err := e.client.getIteration(iterationID)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		return fmt.Errorf("iteration %d: %w", iterationID, err)
	}
	_, err = iterationUpsert(iterationUpsertRequest{
		ExternalID: it.ID,
		Name:       it.Name,
		StartDate:  it.StartDate,
		EndDate:    it.EndDate,
		Status:     it.State,
		Workspace:  wsID,
	})
	return err
}

// ensureParentEpic imports a story's parent epic on demand when its mapping
// is missing. The catalog pass runs only at install/reset, so an epic created
// after the last catalog import has no windshift item yet — the core's
// parent_id resolution then rejects every child story with "parent mapping
// not found", and the failed child never retries (cursors advance past
// item-level errors, contract §6). An epic item_lookup is cheap and stays on
// the hot path; on a miss it costs one GET /epics/:id plus the same upsert
// the catalog uses. The returned bool is false when the child must be
// imported parentless: the epic is deleted in Shortcut or tombstoned by the
// sweep, so no mapping will ever exist for it.
func (e *engine) ensureParentEpic(epicID int64) (bool, error) {
	if e.tombstoned("epic", epicID) {
		return false, nil
	}
	lk, err := itemLookup("epic", epicID)
	if err != nil {
		return false, err
	}
	if lk.Found {
		return true, nil
	}
	ep, err := e.client.getEpic(epicID)
	if err != nil {
		return false, fmt.Errorf("parent epic %d: %w", epicID, err)
	}
	resp, err := itemUpsert(e.epicRequest(*ep, e.cfg.WorkspaceID))
	if err != nil {
		return false, fmt.Errorf("parent epic %d: %w", epicID, err)
	}
	e.countUpsert(resp)
	return true, nil
}

func (e *engine) storyRequest(s story, wsID string) itemUpsertRequest {
	title, description := logic.TitleAndDescription(s.Name, s.Description)
	req := itemUpsertRequest{
		ExternalKind:      "story",
		ExternalID:        s.ID,
		ExternalURL:       s.AppURL,
		ExternalUpdatedAt: s.UpdatedAt,
		WorkspaceID:       wsID,
		Title:             title,
		Description:       description,
		// The team board's own column name ("In Development", "Ready for
		// Review", …) so the story lands on the column it sat on in
		// Shortcut; the category-style seed name is the fallback for
		// states outside the cached workflows.
		StatusName: e.storyStatusName(s.WorkflowStateID),
		// Always "Story": Windshift's hierarchy only allows a level-2 Story
		// under a level-1 Epic (bug/task are level 3 and the create
		// validator rejects them as epic children), and Shortcut's
		// story_type is a cosmetic tag with no hierarchy meaning.
		ItemTypeName: "Story",
		ProjectName:  e.projectName[s.ProjectID],
		Labels:       s.labelNames(),
		LabelMode:    e.cfg.LabelMode,
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
func (e *engine) runComments(metas []storyMeta) (bool, error) {
	for i := e.st.CommentIdx; i < len(metas); i++ {
		if !e.budget.Remaining() {
			return true, nil
		}
		m := metas[i]
		e.st.CommentIdx = i + 1
		if e.tombstoned("story", m.ID) {
			continue
		}
		// Budget check per story, not only per comment: most stories have
		// zero comments, so the inner loop check never fires and the fetch
		// itself would run past the wasm deadline — the module dies before
		// any save and the pass never advances.
		if !e.budget.Remaining() {
			e.st.CommentIdx = i
			return true, nil
		}
		incomplete, err := e.importStoryComments(m.ID)
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
func (e *engine) importStoryComments(storyID int64) (bool, error) {
	if e.cfg.DryRun {
		cs, err := e.client.listStoryComments(storyID)
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
		logInfo("dry-run: story " + strconv.FormatInt(storyID, 10) + " has " + strconv.Itoa(live) + " live comments")
		return false, nil
	}
	lk, err := itemLookup("story", storyID)
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
	cs, err := e.client.listStoryComments(storyID)
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

// errBudgetPause is the sentinel returned by per-item callbacks when the
// tick budget is exhausted. Unlike transport aborts it carries no error
// state — progress is saved and the work resumes at the cursor next tick.
var errBudgetPause = errors.New("tick budget exhausted")

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
