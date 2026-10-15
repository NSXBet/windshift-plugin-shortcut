package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"shortcut/logic"
)

// Full-account dump: a resumable export path that captures every Shortcut
// entity the plugin knows about — including data the incremental sync never
// touches (members, milestones, archived stories, epic comments, raw
// originals) — as one line-oriented NDJSON file. Entities stream into
// ordered KV pieces (shortcut:dump:piece:<n>); the CLI assembles the file
// by GETting /dump/piece?n=<k> for k in [0, pieces). While a dump runs,
// k == pieces serves the open in-memory buffer, so partial files download
// before completion. Piece boundaries may fall anywhere, including
// mid-line: the file is the plain concatenation.
//
// Resume model: the whole cursor lives in the Dump state; each tick runs
// within a budget, pauses between entities, and re-fetches its current
// stream from Skip next tick. List endpoints are re-GET-ted per tick and
// Skip elements are decoded-and-discarded, so a long /epics pass pays its
// fetch cost every tick — accepted, because the element bodies are never
// re-fetched (only the one ~4 MB list payload).
//
// Line shapes (restore attaches story/epic comments to the most recent
// matching entity line, which is why comments follow their parent inline):
//
//	{"type":"members","data":{...}}
//	{"type":"comments","story_id":123,"data":{...}}
//	{"type":"epic_comments","epic_id":45,"data":{...}}
//	{"type":"end","filename":...,"pieces":N,"counts":{...}}

const (
	// dumpPieceMax caps a piece's body. Buffers flush to KV at or above
	// this size; the open buffer also lives in the Dump state, so the cap
	// bounds the state's own size too.
	dumpPieceMax = 256 * 1024
	// dumpWindowStep tiles the story scan like the sync backfill, but a
	// full dump must cover every updated_at exactly once, so the windows
	// abut instead of overlapping; the scan runs newest-first.
	dumpWindowStep = 24 * time.Hour
	// dumpWindowStepMax caps the empty-window span growth: 16 weeks still
	// fits both search endpoints (window bounds are just timestamps) while
	// cutting a 2019-vs-2000 org gap to ~a dozen ticks.
	dumpWindowStepMax = 112 * 24 * time.Hour
)

// dumpOrigin predates any real Shortcut org — the NSX org is a 2019 import
// of a still older one, so no story predates the century. A var: time.Date
// isn't constant.
var dumpOrigin = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)

// dumpPhases is the ordered phase list; Dump.Phase names the current one.
// Iteration bodies never get a phase of their own: freezeWindow fetches and
// emits each body inline BEFORE its window's story lines, because restore
// resolves the story→sprint link at story-upsert time — an iteration line
// after its story would silently drop the link. Member bodies likewise:
// each story's unseen assignees are emitted just before the story line (the
// org roster is unbounded and never fully listed — the sync imports members
// the same on-demand way).
var dumpPhases = []string{
	dumpPhaseMeta, dumpPhaseTeams, dumpPhaseWorkflows,
	dumpPhaseEpicWfs, dumpPhaseProjects, dumpPhaseMilestones, dumpPhaseEpics,
	dumpPhaseStories, dumpPhaseDone,
}

// errDumpPause unwinds a streaming callback when the tick budget ran out;
// run() converts it into "not done, resume next tick".
var errDumpPause = errors.New("dump budget pause")

// dumpRunner executes one tick of dump work within a budget.
type dumpRunner struct {
	cfg    *Config
	st     *Dump
	client *shortcutClient
	budget logic.TickBudget
	now    func() time.Time
}

func (d *dumpRunner) remaining() bool { return d.budget.DeadlineMs() > 0 }

// dumpLine assembles one NDJSON line, optionally keyed by a parent id.
func dumpLine(typ, key string, id int64, data json.RawMessage) string {
	b := strings.Builder{}
	b.WriteString(`{"type":"`)
	b.WriteString(typ)
	b.WriteString(`"`)
	if key != "" {
		b.WriteString(`,"`)
		b.WriteString(key)
		b.WriteString(`":`)
		b.WriteString(strconv.FormatInt(id, 10))
	}
	b.WriteString(`,"data":`)
	b.Write(data)
	b.WriteString("}\n")
	return b.String()
}

// emit appends a line to the open piece and bumps its count. Raw passthrough
// keeps the dump byte-faithful to Shortcut's payloads — restore decodes the
// same wire structs the sync uses, so nothing is re-modeled here.
func (d *dumpRunner) emit(typ, key string, id int64, data json.RawMessage) {
	c := &d.st.Counts
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
	d.st.Buf += dumpLine(typ, key, id, data)
	if len(d.st.Buf) >= dumpPieceMax {
		d.flushBuf()
	}
}

// flushBuf writes the open piece to KV. Piece writes are idempotent under
// crash-rewrite: the cursor state is saved at tick end, so a lost save just
// makes the next tick re-emit the same piece key with regenerated content.
func (d *dumpRunner) flushBuf() {
	if d.st.Buf == "" {
		return
	}
	if err := kvSetString(dumpPieceKey(d.st.Pieces), d.st.Buf); err != nil {
		logInfo("dump piece write failed: " + truncateSnippet(err.Error(), 200))
	}
	d.st.Pieces++
	d.st.Buf = ""
}

// run advances phases until the budget runs out or the dump completes.
func (d *dumpRunner) run() error {
	for d.remaining() {
		var done bool
		var err error
		switch d.st.Phase {
		case dumpPhaseMeta:
			d.emitMeta()
			d.st.Phase = dumpPhaseTeams
			continue
		case dumpPhaseTeams:
			done, err = d.streamList("/teams", "teams")
		case dumpPhaseWorkflows:
			done, err = d.streamList("/workflows", "workflows")
		case dumpPhaseEpicWfs:
			done, err = d.streamListTolerant("/epic-workflows", "epic_workflows")
		case dumpPhaseProjects:
			done, err = d.streamList("/projects", "projects")
		case dumpPhaseMilestones:
			done, err = d.streamListTolerant("/milestones", "milestones")
		case dumpPhaseEpics:
			done, err = d.streamEpics()
		case dumpPhaseStories:
			done, err = d.streamStories()
		case dumpPhaseDone:
			return nil
		}
		if err != nil {
			// Rate exhaustion mid-tick pauses like the time budget: the
			// cursor advanced in memory, the tick handler saves it, and
			// the next tick resumes with a fresh limiter window.
			var rl *errRateLimit
			if errors.As(err, &rl) {
				return nil
			}
			return err
		}
		if !done {
			return nil // budget pause; the tick handler saves the cursor
		}
		if d.st.Phase == dumpPhaseDone {
			// streamStories finished the dump (finish sets the phase);
			// advancing past "done" would index past dumpPhases.
			return nil
		}
		d.st.Skip = 0
		d.st.Phase = dumpNextPhase(d.st.Phase)
	}
	return nil
}

func dumpNextPhase(p string) string {
	for i, name := range dumpPhases {
		if name == p {
			return dumpPhases[i+1]
		}
	}
	return dumpPhaseDone
}

// emitMeta writes the file's first line. The filename comes from the start
// timestamp, so the CLI can name the file before the first piece exists.
func (d *dumpRunner) emitMeta() {
	meta, _ := json.Marshal(map[string]any{
		"filename":   d.st.Filename,
		"started_at": d.st.StartedAt,
		"origin":     d.st.Origin,
		"version":    1,
	})
	d.emit("meta", "", 0, meta)
}

// streamList streams one full-list endpoint element by element. elements
// before Skip are decoded and discarded (Skip = next unprocessed index).
func (d *dumpRunner) streamList(path, typ string) (bool, error) {
	done := true
	err := d.client.eachList(path, func(dec *json.Decoder) error {
		if d.st.Skip > 0 {
			var discard json.RawMessage
			return dec.Decode(&discard)
		}
		if !d.remaining() {
			done = false
			return errDumpPause
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		d.emit(typ, "", 0, raw)
		d.st.Skip++
		return nil
	})
	if errors.Is(err, errDumpPause) {
		return false, nil
	}
	return done, err
}

// streamListTolerant treats a 404 as an empty phase (epic-workflows and
// milestones only exist on some orgs).
func (d *dumpRunner) streamListTolerant(path, typ string) (bool, error) {
	done, err := d.streamList(path, typ)
	if err != nil && isNotFound(err) {
		return true, nil
	}
	return done, err
}

// streamEpics emits each epic, then that epic's comments inline. A budget
// pause mid-comments rewinds to the epic itself: the epic line and its
// comments are re-emitted next tick — restore upserts are idempotent, so
// duplicates cost fetch time only.
func (d *dumpRunner) streamEpics() (bool, error) {
	done := true
	err := d.client.eachList("/epics", func(dec *json.Decoder) error {
		if d.st.Skip > 0 {
			var discard json.RawMessage
			return dec.Decode(&discard)
		}
		if !d.remaining() {
			done = false
			return errDumpPause
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		d.emit("epics", "", 0, raw)
		var id struct {
			ID int64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &id); err != nil || id.ID == 0 {
			logInfo("dump epics: element without id, comments skipped")
			d.st.Skip++
			return nil
		}
		commentsDone, err := d.streamEpicComments(id.ID)
		if err != nil {
			return err
		}
		if !commentsDone {
			done = false
			return errDumpPause
		}
		d.st.Skip++
		return nil
	})
	if errors.Is(err, errDumpPause) {
		return false, nil
	}
	return done, err
}

// streamEpicComments streams one epic's comment list; the epic itself is
// already in the file, so the resume story is "re-run the whole epic".
func (d *dumpRunner) streamEpicComments(epicID int64) (bool, error) {
	done := true
	path := "/epics/" + strconv.FormatInt(epicID, 10) + "/comments"
	err := d.client.eachList(path, func(dec *json.Decoder) error {
		if !d.remaining() {
			done = false
			return errDumpPause
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		d.emit("epic_comments", "epic_id", epicID, raw)
		return nil
	})
	if errors.Is(err, errDumpPause) {
		return false, nil
	}
	if isNotFound(err) { // epic deleted between list and comments
		return true, nil
	}
	return done, err
}

// curSpan is the current search span: the base step until the stories pass
// starts skipping empty gap years, then powers of two up to the cap.
func (d *Dump) curSpan() time.Duration {
	if d.WindowSpan > 0 {
		return time.Duration(d.WindowSpan) * time.Second
	}
	return dumpWindowStep
}

// bumpSpan doubles the span after an empty frozen window. freezeWindow's
// two searches re-run on the (now doubled) span next iteration, so a
// decades-long gap before an org's first story costs ~log2 span ticks,
// not one per empty day.
func (d *Dump) bumpSpan() {
	next := d.curSpan() * 2
	if next > dumpWindowStepMax {
		next = dumpWindowStepMax
	}
	d.WindowSpan = int64(next / time.Second)
}

// stepWindow moves the stories cursor one span backwards (the scan runs
// newest-first, windows abutting at each span). Crossing dumpOrigin arms
// AtOrigin: the crossing window is scanned, the next loop-top finishes.
func (d *Dump) stepWindow(ws time.Time) {
	next := ws.Add(-d.curSpan())
	if !next.After(dumpOrigin) {
		d.AtOrigin = true
	}
	d.WindowStart = next.Format(time.RFC3339)
}

// emitStoryMembers emits each of the story's unseen assignees' member bodies
// inline BEFORE the story line, mirroring the iteration-freeze invariant:
// restore resolves story→user links at story-upsert time. A 404 is marked
// seen and dropped exactly like the live on-demand import path (member
// deleted between owner_ids and the body fetch). Pausing mid-story is safe:
// Skip is unchanged, so the next tick re-runs the story and SeenMembers
// skips members already emitted.
func (d *dumpRunner) emitStoryMembers(raw json.RawMessage, storyID int64) error {
	if d.st.SeenMembers == nil {
		d.st.SeenMembers = make(map[string]bool)
	}
	var s story
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("dump story %d body: %w", storyID, err)
	}
	for _, id := range s.OwnerIDs {
		if d.st.SeenMembers[id] {
			continue
		}
		if !d.remaining() {
			return errDumpPause
		}
		mraw, err := d.client.do("GET", "/members/"+url.PathEscape(id), nil)
		if err != nil {
			if !isNotFound(err) {
				return err
			}
		} else {
			d.emit("members", "", 0, mraw)
		}
		d.st.SeenMembers[id] = true
	}
	return nil
}

// streamStories tiles [origin, now) with abutting windows, scanned
// newest-first (base 24h, span-doubled across empty gap years). Each window
// is searched twice (active + archived), condensed to metas, frozen into
// the state, then drained one story per fetch with that story's comments
// inline. A pause mid-story rewinds to the story: its line and comments are
// re-emitted next tick (idempotent restore), matching the epic behavior.
func (d *dumpRunner) streamStories() (bool, error) {
	for {
		if !d.remaining() {
			return false, nil
		}
		if d.st.AtOrigin {
			d.finish()
			return true, nil
		}
		if d.st.WindowStart == "" {
			// First stories window: [now - base span, now). The scan runs
			// backwards from now, so the freshest stories — the visible
			// proof the dump works — land in the first ticks, and the
			// empty pre-org gap years cost their span-doubled windows last.
			d.st.WindowStart = d.now().Add(-d.st.curSpan()).Format(time.RFC3339)
		}
		ws, err := time.Parse(time.RFC3339, d.st.WindowStart)
		if err != nil {
			return false, fmt.Errorf("dump window start %q: %w", d.st.WindowStart, err)
		}
		we := ws.Add(d.st.curSpan())
		if d.st.WindowStories == "" {
			if err := d.freezeWindow(ws, we); err != nil {
				if errors.Is(err, errDumpPause) {
					return false, nil // window stays unfrozen; searches re-run next tick
				}
				return false, err
			}
			if d.st.WindowStories == "" {
				// empty window: step back and keep going within this tick
				d.st.stepWindow(ws)
				d.st.bumpSpan()
				continue
			}
		}
		var metas []storyMeta
		if err := json.Unmarshal([]byte(d.st.WindowStories), &metas); err != nil {
			return false, fmt.Errorf("dump window metas: %w", err)
		}
		for d.st.Skip < len(metas) {
			if !d.remaining() {
				return false, nil
			}
			m := metas[d.st.Skip]
			raw, err := d.client.do("GET", "/stories/"+strconv.FormatInt(m.ID, 10), nil)
			if err != nil {
				if isNotFound(err) { // deleted between search and body fetch
					d.st.Skip++
					continue
				}
				return false, err
			}
			if err := d.emitStoryMembers(raw, m.ID); err != nil {
				if errors.Is(err, errDumpPause) {
					return false, nil // resume re-runs this story; seen members skip
				}
				return false, err
			}
			d.emit("stories", "", 0, raw)
			commentsDone, err := d.streamStoryComments(m.ID)
			if err != nil {
				return false, err
			}
			if !commentsDone {
				return false, nil // resume re-runs this story from its line
			}
			d.st.Skip++
		}
		d.st.stepWindow(ws)
		d.st.WindowStories = ""
		d.st.Skip = 0
		d.st.WindowSpan = 0
	}
}

// freezeWindow runs the two searches and fetches any not-yet-seen iteration
// bodies BEFORE condensing, so iteration lines precede their stories. If
// the budget runs out mid-iteration-fetch, the window stays unfrozen and
// the searches re-run next tick — two wasted searches, simpler cursor.
func (d *dumpRunner) freezeWindow(ws, we time.Time) error {
	stories, err := d.client.searchStoriesArchived(ws, we, false)
	if err != nil {
		return err
	}
	archived, err := d.client.searchStoriesArchived(ws, we, true)
	if err != nil {
		return err
	}
	stories = append(stories, archived...)
	for _, s := range stories {
		if s.IterationID == nil || *s.IterationID == 0 || d.seenIteration(*s.IterationID) {
			continue
		}
		if !d.remaining() {
			return errDumpPause
		}
		raw, err := d.client.do("GET", "/iterations/"+strconv.FormatInt(*s.IterationID, 10), nil)
		if err != nil {
			if isNotFound(err) { // story's iteration vanished; story imports without it
				continue
			}
			return err
		}
		d.emit("iterations", "", 0, raw)
		d.st.IterationIDs = append(d.st.IterationIDs, *s.IterationID)
	}
	if len(stories) == 0 {
		return nil
	}
	d.st.WindowStories = string(condenseStories(stories))
	return nil
}

func (d *dumpRunner) seenIteration(id int64) bool {
	for _, v := range d.st.IterationIDs {
		if v == id {
			return true
		}
	}
	return false
}

// streamStoryComments streams one story's comment list inline after its
// story line.
func (d *dumpRunner) streamStoryComments(storyID int64) (bool, error) {
	done := true
	path := "/stories/" + strconv.FormatInt(storyID, 10) + "/comments"
	err := d.client.eachList(path, func(dec *json.Decoder) error {
		if !d.remaining() {
			done = false
			return errDumpPause
		}
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return err
		}
		d.emit("comments", "story_id", storyID, raw)
		return nil
	})
	if errors.Is(err, errDumpPause) {
		return false, nil
	}
	if isNotFound(err) { // story deleted between body and comments
		return true, nil
	}
	return done, err
}

// finish writes the end line (with a predictable piece total) and closes
// the dump. Predictability: the open buffer is flushed first, so the end
// line opens the last piece and the total is Pieces+1 before it's emitted.
func (d *dumpRunner) finish() {
	d.flushBuf()
	end, _ := json.Marshal(map[string]any{
		"filename":    d.st.Filename,
		"started_at":  d.st.StartedAt,
		"finished_at": d.now().UTC().Format(time.RFC3339),
		"pieces":      d.st.Pieces + 1,
		"counts":      d.st.Counts,
	})
	d.emit("end", "", 0, end)
	d.flushBuf()
	d.st.FinishedAt = d.now().UTC().Format(time.RFC3339)
	d.st.Phase = dumpPhaseDone
}

// --- HTTP handlers (admin panel + tick) ---

func dumpStatusResponse(d *Dump) HTTPResponse {
	body, _ := json.Marshal(map[string]any{
		"phase":       d.Phase,
		"started_at":  d.StartedAt,
		"finished_at": d.FinishedAt,
		"filename":    d.Filename,
		"pieces":      d.Pieces,
		"counts":      d.Counts,
		"error":       d.Error,
	})
	return HTTPResponse{StatusCode: 200, Headers: jsonHeaders, Body: string(body)}
}

// handleDumpStart begins a fresh dump. An in-progress dump blocks until it
// finishes or is discarded; {"force":true} discards and restarts.
func handleDumpStart(body string) HTTPResponse {
	cfg, err := loadConfig()
	if err != nil {
		return errorResponse(500, "config load failed")
	}
	if cfg == nil {
		return errorResponse(400, "not configured — save the connection form first")
	}
	var req struct {
		Force bool `json:"force"`
	}
	if body != "" {
		if err := json.Unmarshal([]byte(body), &req); err != nil {
			return errorResponse(400, "invalid body: "+truncateSnippet(err.Error(), 200))
		}
	}
	prev, err := loadDump()
	if err != nil {
		return errorResponse(500, "dump state corrupt; discard it first")
	}
	if prev != nil && prev.Phase != dumpPhaseDone && !req.Force {
		return errorResponse(409, "a dump is already running; discard it first (POST /dump/discard)")
	}
	if prev != nil {
		discardDumpPieces(prev)
	}
	started := now().UTC().Format(time.RFC3339)
	d := &Dump{
		Phase:     dumpPhaseMeta,
		V:         2,
		StartedAt: started,
		Filename:  "shortcut-dump-" + strings.ReplaceAll(started, ":", "") + ".ndjson",
		Origin:    dumpOrigin.Format(time.RFC3339),
	}
	if err := saveDump(d); err != nil {
		return errorResponse(500, "dump save failed")
	}
	logInfo("dump started: " + d.Filename)
	return dumpStatusResponse(d)
}

// runDumpTick is one budgeted slice of dump work: load cursor + config,
// run, record the error snippet, persist, and return the status payload.
// Shared by the /dump/tick route and the scheduled dump_tick export (the
// manifest ticks dumps every 10s so org-scale dumps progress unattended).
func runDumpTick() HTTPResponse {
	cfg, err := loadConfig()
	if err != nil {
		return errorResponse(500, "config load failed")
	}
	if cfg == nil {
		return errorResponse(400, "not configured — save the connection form first")
	}
	d, err := loadDump()
	if err != nil {
		return errorResponse(500, "dump state corrupt (discard to reset)")
	}
	if d == nil {
		return errorResponse(409, "no dump started (POST /dump/start)")
	}
	if d.Phase == dumpPhaseDone {
		return dumpStatusResponse(d)
	}
	r := &dumpRunner{
		cfg:    cfg,
		st:     d,
		client: newShortcutClient(cfg),
		budget: logic.NewTickBudget(time.Now(), tickBudget),
		now:    now,
	}
	r.client.remainingMs = func() int64 { return r.budget.DeadlineMs() }
	if err := r.run(); err != nil {
		d.Error = truncateSnippet(err.Error(), 300)
		logInfo("dump error: " + d.Error)
	} else {
		d.Error = ""
	}
	if err := saveDump(d); err != nil {
		return errorResponse(500, "dump save failed")
	}
	return dumpStatusResponse(d)
}

//go:wasmexport dump_tick
func dumpTick() {
	runDumpTick()
}

func handleDumpStatus() HTTPResponse {
	d, err := loadDump()
	if err != nil {
		return errorResponse(500, "dump state corrupt (discard to reset)")
	}
	if d == nil {
		return errorResponse(404, "no dump started")
	}
	return dumpStatusResponse(d)
}

// handleDumpPiece serves one assembled-file piece. The body is raw NDJSON
// bytes — a possibly mid-line slice of the logical file. Piece n == Pieces
// (only while a dump runs) is the open in-memory buffer, so a partial file
// can be downloaded before the dump finishes.
func handleDumpPiece(query map[string]string) HTTPResponse {
	d, err := loadDump()
	if err != nil {
		return errorResponse(500, "dump state corrupt (discard to reset)")
	}
	if d == nil {
		return errorResponse(404, "no dump started")
	}
	n, err := strconv.Atoi(query["n"])
	if err != nil || n < 0 {
		return errorResponse(400, "bad piece number")
	}
	if n == d.Pieces && d.Phase != dumpPhaseDone {
		return HTTPResponse{StatusCode: 200, Headers: map[string]string{"Content-Type": "application/x-ndjson"}, Body: d.Buf}
	}
	if n >= d.Pieces {
		return errorResponse(400, fmt.Sprintf("piece out of range: want 0..%d", d.Pieces-1))
	}
	body, ok, err := kvGetString(dumpPieceKey(n))
	if err != nil {
		return errorResponse(500, "piece read failed")
	}
	if !ok {
		return errorResponse(500, fmt.Sprintf("piece %d missing from KV (state says it exists)", n))
	}
	return HTTPResponse{
		StatusCode: 200,
		Headers:    map[string]string{"Content-Type": "application/x-ndjson"},
		Body:       body,
	}
}

// handleDumpDiscard deletes the dump state and all pieces.
func handleDumpDiscard() HTTPResponse {
	d, err := loadDump()
	if err != nil {
		return errorResponse(500, "dump state corrupt; deleting it anyway")
	}
	if d != nil {
		discardDumpPieces(d)
	}
	if err := kvSetString(kvDumpKey, ""); err != nil {
		return errorResponse(500, "dump state delete failed")
	}
	logInfo("dump discarded")
	return HTTPResponse{StatusCode: 204, Headers: jsonHeaders}
}

func discardDumpPieces(d *Dump) {
	for i := range d.Pieces {
		if err := kvDelete(dumpPieceKey(i)); err != nil {
			logInfo(fmt.Sprintf("dump piece %d delete failed: %v", i, err))
		}
	}
}
