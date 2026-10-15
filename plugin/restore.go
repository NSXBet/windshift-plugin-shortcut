package main

// Restore engine: replays a full-account dump (the NDJSON file assembled
// from /dump/piece) into windshift. The CLI posts line-aligned chunks to
// POST /restore/chunk?seq=<n>; every chunk is re-posted until the response
// says chunk_done, so a budget pause, transport abort, or killed module
// resumes at the same line. All writes run through the sync engine's upsert
// paths with offline=true — no Shortcut traffic, and the item_lookup +
// external_updated_at guards make replays no-ops. The dump's catalog lines
// rebuild the state→type/team maps the live engine fetches from Shortcut at
// buildMaps time (dump file order guarantees every catalog line precedes
// the stories that need it).
//
// The host time must be after the dump's story window (restore of a fresh
// dump into a fresh windshift host always is).

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"shortcut/logic"
)

// errRestoreReject refuses a chunk without touching state: the CLI posted a
// file that cannot be this restore's (missing meta, filename mismatch,
// wrong order). It is a POST response error, not a state error — the caller
// may correct the request and re-post immediately.
var errRestoreReject = errors.New("restore chunk rejected")

// restoreRunner executes one /restore/chunk invocation within a budget. The
// offline engine shim carries counts, last errors, and the member/iteration
// lookups; maps install lazily on the first epic/story line, after the
// catalog lines have buffered.
type restoreRunner struct {
	cfg    *Config
	st     *Restore
	e      *engine
	budget logic.TickBudget
	now    func() time.Time
}

func (r *restoreRunner) remaining() bool { return r.budget.DeadlineMs() > 0 }

// handleRestoreStatus reports the restore cursor (panel + CLI polling).
func handleRestoreStatus() HTTPResponse {
	r, err := loadRestore()
	if err != nil {
		return errorResponse(500, "restore state corrupt (discard to reset)")
	}
	if r == nil {
		return errorResponse(404, "no restore started")
	}
	return restoreStatusResponse(r)
}

// handleRestoreChunk consumes one line-aligned chunk of the dump file. The
// body is raw NDJSON; ?seq= must match the state's Seq so two concurrent
// CLIs cannot interleave chunks. Chunks re-post until chunk_done; the end
// line flips the restore to done (idempotent re-posts return the status).
func handleRestoreChunk(query map[string]string, body string) HTTPResponse {
	cfg, err := loadConfig()
	if err != nil {
		return errorResponse(500, "config load failed")
	}
	if cfg == nil {
		return errorResponse(400, "not configured — save the connection form first")
	}
	r, err := loadRestore()
	if err != nil {
		return errorResponse(500, "restore state corrupt (discard to reset)")
	}
	if r != nil && r.Phase == restorePhaseDone {
		return restoreStatusResponse(r) // missed done reply: idempotent re-post
	}
	seq, err := strconv.Atoi(query["seq"])
	if err != nil || seq < 0 {
		return errorResponse(400, "missing or invalid ?seq=")
	}
	if r != nil && seq != r.Seq {
		return errorResponse(409, fmt.Sprintf("out-of-order chunk: state wants seq %d", r.Seq))
	}
	if body == "" {
		return errorResponse(400, "empty chunk body")
	}
	if r == nil {
		r = &Restore{Phase: restorePhaseStream, StartedAt: now().UTC().Format(time.RFC3339)}
	}
	run := &restoreRunner{
		cfg:    cfg,
		st:     r,
		budget: logic.NewTickBudget(time.Now(), tickBudget),
		now:    now,
	}
	run.e = run.engineFor()
	resp, herr := run.consume(body)
	if herr != nil {
		return errorResponse(400, truncateSnippet("chunk rejected: "+herr.Error(), 200))
	}
	if err := saveRestore(r); err != nil {
		return errorResponse(500, "restore state save failed")
	}
	return resp
}

// consume processes chunk lines until the budget or the chunk end. Durable
// progress persists only at the end of the invocation: a wasm kill loses
// the whole chunk, and the CLI's re-post replays it through the upsert
// guards as a no-op.
func (r *restoreRunner) consume(body string) (HTTPResponse, error) {
	r.st.Error = ""
	lines := strings.Split(strings.TrimSuffix(body, "\n"), "\n")
	idx := r.st.ChunkIdx
	for idx < len(lines) {
		if line := lines[idx]; line == "" {
			idx++
			r.st.ChunkIdx = idx
			continue
		}
		if !r.remaining() {
			break // budget: the same chunk re-posts and resumes here
		}
		if err := r.process(lines[idx]); err != nil {
			if errors.Is(err, errRestoreReject) {
				return HTTPResponse{}, err
			}
			if isAborted(err) || errors.Is(err, errBudgetPause) {
				// Transport-level or budget pause: the line stays
				// unconsumed; the re-posted chunk retries it.
				r.st.Error = truncateSnippet(err.Error(), 300)
				break
			}
			r.e.recordError(err) // unexpected item-level path: keep moving
		}
		idx++
		r.st.ChunkIdx = idx
		if r.st.Phase == restorePhaseDone {
			break
		}
	}
	// Merge the shim's counters into the persisted state: engineFor starts
	// each invocation from a fresh state, so an overwrite would drop every
	// earlier chunk's tallies. Re-posts cannot double-count — a wasm kill
	// never reaches this flush, budget pauses resume at ChunkIdx, and
	// stale-seq re-posts are rejected before processing.
	r.st.Counts.Created += r.e.st.Counts.Created
	r.st.Counts.Updated += r.e.st.Counts.Updated
	r.st.Counts.Skipped += r.e.st.Counts.Skipped
	r.st.Counts.Errors += r.e.st.Counts.Errors
	r.st.Counts.Comments += r.e.st.Counts.Comments
	r.st.LastErrors = r.e.st.LastErrors
	done := r.st.Phase == restorePhaseDone
	chunkDone := done || idx >= len(lines)
	if chunkDone && !done {
		r.st.Seq++
		r.st.ChunkIdx = 0
	}
	return restoreChunkResponse(r.st, chunkDone, done), nil
}

// process consumes one dump line. Item-level failures are recorded via the
// engine and the line consumed (contract §6 semantics); transport aborts
// and hard rejects return so the chunk re-posts or is refused.
func (r *restoreRunner) process(line string) error {
	var ln struct {
		Type    string          `json:"type"`
		StoryID *int64          `json:"story_id"`
		EpicID  *int64          `json:"epic_id"`
		Data    json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal([]byte(line), &ln); err != nil || ln.Type == "" {
		r.e.recordError(fmt.Errorf("bad line: %v", err))
		return nil
	}
	if r.st.Filename == "" && ln.Type != "meta" {
		return fmt.Errorf("first chunk must open with the dump's meta line: %w", errRestoreReject)
	}
	switch ln.Type {
	case "meta":
		return r.processMeta(ln.Data)
	case "members", "teams", "workflows", "epic_workflows", "projects", "iterations":
		return r.buffer(ln.Type, ln.Data)
	case "milestones":
		r.bump("milestones") // no windshift mapping exists; counted, skipped
		return nil
	case "epics":
		return r.processEpic(ln.Data)
	case "epic_comments":
		return r.processComment("epic_comments", ln.EpicID, ln.Data)
	case "stories":
		return r.processStory(ln.Data)
	case "comments":
		return r.processComment("comments", ln.StoryID, ln.Data)
	case "end":
		return r.processEnd(ln.Data)
	default:
		r.e.recordError(fmt.Errorf("unknown dump line type %q skipped", ln.Type))
		return nil
	}
}

// processMeta opens (or verifies) the dump identity.
func (r *restoreRunner) processMeta(data []byte) error {
	var m struct {
		Filename string `json:"filename"`
	}
	err := json.Unmarshal(data, &m)
	if err != nil || m.Filename == "" {
		return fmt.Errorf("unreadable meta line (%v): %w", err, errRestoreReject)
	}
	if r.st.Filename != "" && r.st.Filename != m.Filename {
		return fmt.Errorf("chunk's dump %q mismatches the in-flight restore of %q: %w",
			m.Filename, r.st.Filename, errRestoreReject)
	}
	r.st.Filename = m.Filename
	return nil
}

// buffer accumulates catalog lines in the restore state. Members feed the
// story-time assignee lookup immediately (late members appear after stories
// in a resumed window); the map install mirrors engineFor's.
func (r *restoreRunner) buffer(typ string, data []byte) error {
	switch typ {
	case "members":
		var m shortcutMember
		if err := json.Unmarshal(data, &m); err != nil || m.ID == "" {
			r.e.recordError(fmt.Errorf("member line: %v", err))
			return nil
		}
		r.st.Members = append(r.st.Members, m)
		if r.e != nil {
			r.e.membersByID[m.ID] = &r.st.Members[len(r.st.Members)-1]
		}
	case "teams":
		var t team
		if err := json.Unmarshal(data, &t); err != nil || t.ID == 0 {
			r.e.recordError(fmt.Errorf("team line: %v", err))
			return nil
		}
		if t.Workflow != nil {
			t.States = t.Workflow.States // same flatten buildMaps does
		}
		r.st.Teams = append(r.st.Teams, t)
	case "workflows", "epic_workflows":
		var wf workflow
		if err := json.Unmarshal(data, &wf); err != nil || wf.ID == 0 {
			r.e.recordError(fmt.Errorf("%s line: %v", typ, err))
			return nil
		}
		if typ == "workflows" {
			r.st.Workflows = append(r.st.Workflows, wf)
		} else {
			r.st.EpicWorkflows = append(r.st.EpicWorkflows, wf)
		}
	case "projects":
		var p project
		if err := json.Unmarshal(data, &p); err != nil || p.ID == 0 {
			r.e.recordError(fmt.Errorf("project line: %v", err))
			return nil
		}
		r.st.Projects = append(r.st.Projects, p)
	case "iterations":
		var it shortcutIteration
		if err := json.Unmarshal(data, &it); err != nil || it.ID == 0 {
			r.e.recordError(fmt.Errorf("iteration line: %v", err))
			return nil
		}
		r.st.Iterations = append(r.st.Iterations, it)
		if r.e != nil {
			r.e.iterationsByID[it.ID] = &r.st.Iterations[len(r.st.Iterations)-1]
		}
	}
	r.bump(typ)
	return nil
}

// ready installs the dump-derived maps on the first epic or story line, so
// state resolution sees every catalog line the file shipped before them.
func (r *restoreRunner) ready() *engine {
	if !r.st.MapsReady {
		r.e.adoptMapsCache(r.mapsPayload())
		r.st.MapsReady = true
	}
	return r.e
}

// engineFor builds the offline engine shim: the real engine's upsert paths
// with members/iterations resolved from the dump's own buffers instead of
// the Shortcut API.
func (r *restoreRunner) engineFor() *engine {
	e := &engine{cfg: r.cfg, st: &State{}, budget: r.budget, now: r.now, offline: true}
	e.membersByID = make(map[string]*shortcutMember, len(r.st.Members))
	for i := range r.st.Members {
		e.membersByID[r.st.Members[i].ID] = &r.st.Members[i]
	}
	e.iterationsByID = make(map[int64]*shortcutIteration, len(r.st.Iterations))
	for i := range r.st.Iterations {
		e.iterationsByID[r.st.Iterations[i].ID] = &r.st.Iterations[i]
	}
	if r.st.MapsReady {
		e.adoptMapsCache(r.mapsPayload())
	}
	return e
}

// mapsPayload serializes the buffered catalog exactly like buildMaps writes
// MapsJSON (adoptMapsCache's comment: the restore engine serializes the
// same shape from dump catalog records).
func (r *restoreRunner) mapsPayload() mapsPayload {
	p := mapsPayload{
		StateType:     make(map[int64]string, 64),
		StateName:     make(map[int64]string, 64),
		EpicStateType: make(map[int64]string, 16),
		ProjectName:   make(map[int64]string, 64),
		TeamByState:   make(map[int64]int64, 64),
	}
	for i := range r.st.Workflows {
		wf := &r.st.Workflows[i]
		for _, st := range wf.States {
			p.StateType[st.ID] = st.Type
			p.StateName[st.ID] = st.Name
			if wf.TeamID != nil {
				// A story's team is its workflow's team (team mode).
				p.TeamByState[st.ID] = *wf.TeamID
			}
		}
	}
	if r.cfg.WorkspaceMode == "team" {
		p.Teams = make(map[int64]teamMeta, len(r.st.Teams))
		for i := range r.st.Teams {
			t := &r.st.Teams[i]
			p.Teams[t.ID] = teamMeta{Name: t.Name, States: t.States}
		}
	}
	for i := range r.st.EpicWorkflows {
		for _, st := range r.st.EpicWorkflows[i].States {
			p.EpicStateType[st.ID] = st.Type
		}
	}
	for i := range r.st.Projects {
		p.ProjectName[r.st.Projects[i].ID] = r.st.Projects[i].Name
	}
	return p
}

// processEpic imports one epic (the catalog pass equivalent). Epic lines
// precede every story in the file, so child stories find their parent
// mapping in place.
func (r *restoreRunner) processEpic(data []byte) error {
	var ep epic
	if err := json.Unmarshal(data, &ep); err != nil || ep.ID == 0 {
		r.e.recordError(fmt.Errorf("epic line: %v", err))
		return nil
	}
	itemID, err := r.ready().upsertEpicMeta(epicMeta{ID: ep.ID, UpdatedAt: ep.UpdatedAt}, &ep)
	if err != nil {
		return err
	}
	r.bump("epics")
	r.st.CurrentEpicID, r.st.CurrentStoryID, r.st.CurrentItemID = ep.ID, 0, itemID
	return nil
}

// processStory imports one story, then hands its item id to the comment
// lines that follow it inline.
func (r *restoreRunner) processStory(data []byte) error {
	var s story
	if err := json.Unmarshal(data, &s); err != nil || s.ID == 0 {
		r.e.recordError(fmt.Errorf("story line: %v", err))
		return nil
	}
	itemID, err := r.upsertStoryLine(s)
	if err != nil {
		return err
	}
	r.bump("stories")
	r.st.CurrentStoryID, r.st.CurrentEpicID, r.st.CurrentItemID = s.ID, 0, itemID
	return nil
}

// upsertStoryLine mirrors syncStory's lookup-and-skip skeleton with the
// body already in hand (offline: no Shortcut fetch).
func (r *restoreRunner) upsertStoryLine(s story) (string, error) {
	e := r.ready()
	if !e.cfg.DryRun {
		lk, err := itemLookup("story", s.ID)
		if err != nil {
			return "", err
		}
		skip, err := logic.ShouldSkip(lk.Found, lk.LastSyncedAt, s.UpdatedAt)
		if err != nil {
			return "", err
		}
		if skip {
			e.st.Counts.Skipped++
			return lk.ItemID, nil
		}
	}
	return e.upsertStory(s)
}

// processComment imports one comment line for its parent's mapped item.
// typ is the dump's line type ("comments" / "epic_comments"); the parent
// kind derives from it.
func (r *restoreRunner) processComment(typ string, keyID *int64, data []byte) error {
	e := r.ready()
	kind := "story"
	if typ == "epic_comments" {
		kind = "epic"
	}
	if keyID == nil || *keyID == 0 {
		r.e.recordError(errors.New("comment line without parent id"))
		return nil
	}
	var c storyComment
	if err := json.Unmarshal(data, &c); err != nil {
		r.e.recordError(fmt.Errorf("comment line: %v", err))
		return nil
	}
	if e.cfg.DryRun {
		e.st.Counts.Comments++
		return nil
	}
	carryID, carryItem := int64(0), ""
	if kind == "story" {
		carryID, carryItem = r.st.CurrentStoryID, r.st.CurrentItemID
	} else {
		carryID, carryItem = r.st.CurrentEpicID, r.st.CurrentItemID
	}
	itemID, found, err := r.commentTarget(kind, *keyID, carryID, carryItem)
	if err != nil {
		return err
	}
	if !found {
		// The parent imported with a recorded error earlier in the file
		// (or the upsert was skipped); its comments have nothing to attach
		// to — treated as done, same as the live path.
		return nil
	}
	incomplete, err := e.importComments(itemID, []storyComment{c})
	if err != nil {
		return err
	}
	if incomplete {
		return errBudgetPause
	}
	r.bump(typ)
	return nil
}

// commentTarget resolves the parent's windshift item. The carry-over
// (parent was the immediately preceding line) avoids the lookup; a
// carry-miss falls through to item_lookup, whose miss reads "nothing to
// attach to".
func (r *restoreRunner) commentTarget(kind string, keyID, carryID int64, carryItem string) (int, bool, error) {
	if keyID == carryID && carryItem != "" {
		n, err := itemIDInt(carryItem)
		if err != nil {
			return 0, false, fmt.Errorf("%s %d item id: %w", kind, keyID, err)
		}
		return n, true, nil
	}
	lk, err := itemLookup(kind, keyID)
	if err != nil {
		return 0, false, err
	}
	if !lk.Found {
		return 0, false, nil
	}
	n, err := itemIDInt(lk.ItemID)
	if err != nil {
		return 0, false, fmt.Errorf("%s %d item id: %w", kind, keyID, err)
	}
	return n, true, nil
}

// processEnd closes the restore. The counts echo the dump's own end line.
func (r *restoreRunner) processEnd(data []byte) error {
	var m struct {
		FinishedAt string `json:"finished_at"`
	}
	_ = json.Unmarshal(data, &m)
	if m.FinishedAt == "" {
		m.FinishedAt = r.now().UTC().Format(time.RFC3339)
	}
	r.st.FinishedAt = m.FinishedAt
	r.st.Phase = restorePhaseDone
	return nil
}

// bump counts one consumed dump line of the given type (meta/end carry no
// counter).
func (r *restoreRunner) bump(typ string) {
	r.st.TypeCounts.bump(typ)
}

// --- responses ---

// restoreStatusResponse is the status body and the idempotent done reply.
func restoreStatusResponse(r *Restore) HTTPResponse {
	body, _ := json.Marshal(map[string]any{
		"phase":       r.Phase,
		"started_at":  r.StartedAt,
		"finished_at": r.FinishedAt,
		"filename":    r.Filename,
		"seq":         r.Seq,
		"chunk_idx":   r.ChunkIdx,
		"counts":      r.Counts,
		"type_counts": r.TypeCounts,
		"last_errors": r.LastErrors,
		"error":       r.Error,
	})
	return HTTPResponse{StatusCode: 200, Headers: jsonHeaders, Body: string(body)}
}

// restoreChunkResponse tells the CLI what to post next: the same seq again
// when the chunk is unfinished, seq+1 when consumed, done when the end line
// ran.
func restoreChunkResponse(r *Restore, chunkDone, done bool) HTTPResponse {
	next := r.Seq
	if chunkDone && !done {
		next = r.Seq + 1
	}
	body, _ := json.Marshal(map[string]any{
		"chunk_done":  chunkDone,
		"done":        done,
		"seq":         r.Seq,
		"next_seq":    next,
		"chunk_idx":   r.ChunkIdx,
		"counts":      r.Counts,
		"type_counts": r.TypeCounts,
	})
	return HTTPResponse{StatusCode: 200, Headers: jsonHeaders, Body: string(body)}
}

// handleRestoreDiscard deletes the restore state (used after a corrupt
// pause or a completed restore, before restoring again).
func handleRestoreDiscard() HTTPResponse {
	if err := kvSetString(kvRestoreKey, ""); err != nil {
		return errorResponse(500, "restore state delete failed")
	}
	logInfo("restore discarded")
	return HTTPResponse{StatusCode: 204, Headers: jsonHeaders}
}
