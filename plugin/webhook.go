// Delete handling (tk-thf) and operator control routes (tk-swm).
//
// Webhook: Shortcut outgoing webhooks sign each delivery with
// HMAC-SHA256 over the raw UTF-8 body, hex-encoded, in the
// `Payload-Signature` header (https://developer.shortcut.com/api/webhook/v1).
// Story delete/archive events carry no API-side trace, so the handler
// tombstones the id (create a Windshift tombstone comment + KV marker);
// every later tick then skips it (contract §5).
package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"shortcut/logic"
)

// --- wire: Shortcut outgoing webhooks v1 ---

// shortcutWebhook is the envelope of every Shortcut webhook delivery.
type shortcutWebhook struct {
	Actions []shortcutWebhookAction `json:"actions"`
}

type shortcutWebhookAction struct {
	Action     string `json:"action"` // "story_delete", "story_update", ...
	EntityType string `json:"entity_type"`
	ID         int64  `json:"id"`
}

// --- wire: operator control routes (tk-swm) ---

// configUpdate accepts the full config; a nil Token keeps the stored token
// (write-only field — the panel never receives it back, so it can't re-send
// it and blank fields must not erase it).
type configUpdate struct {
	Token         *string `json:"token"`
	Enabled       bool    `json:"enabled"`
	DryRun        bool    `json:"dry_run"`
	WorkspaceID   string  `json:"workspace_id"`
	ProjectIDs    []int64 `json:"project_ids,omitempty"`
	LabelMode     string  `json:"label_mode"`
	ActorUserID   int64   `json:"actor_user_id,omitempty"`
	// WebhookSecret is nil-keep like Token: the GET never echoes it, so a
	// panel save that omits it must not erase the stored secret.
	WebhookSecret *string `json:"webhook_secret,omitempty"`
	BackfillDays  int     `json:"backfill_days,omitempty"`
}

// statusReport is the operator-facing state summary. Token/WebhookSecret
// are echoed only as set/unset booleans (contract §3: write-only secrets).
type statusReport struct {
	Plugin string `json:"plugin"`
	Config struct {
		Enabled      bool    `json:"enabled"`
		DryRun       bool    `json:"dry_run"`
		WorkspaceID  string  `json:"workspace_id"`
		ProjectIDs   []int64 `json:"project_ids,omitempty"`
		LabelMode    string  `json:"label_mode"`
		ActorUserID  int64   `json:"actor_user_id,omitempty"`
		TokenSet     bool    `json:"token_set"`
		WebhookSet   bool    `json:"webhook_secret_set"`
		BackfillDays int     `json:"backfill_days"`
	} `json:"config"`
	State *State `json:"state,omitempty"`
}

// --- webhook handler ---

// handleWebhook verifies the HMAC signature and records tombstones for
// story delete/archive events. Contract responses: 204 accepted, 400
// malformed, 401 signature mismatch/unset secret.
func handleWebhook(req HTTPRequest) HTTPResponse {
	cfg, err := loadConfig()
	if err != nil || cfg == nil || !cfg.verifyWebhookSignature(req.Headers["Payload-Signature"], req.Body) {
		logInfo("webhook: rejected (signature or config unavailable)")
		return HTTPResponse{StatusCode: 401, Headers: jsonHeaders, Body: `{"error":"invalid signature"}`}
	}

	var ev shortcutWebhook
	if err := json.Unmarshal([]byte(req.Body), &ev); err != nil || len(ev.Actions) == 0 {
		return HTTPResponse{StatusCode: 400, Headers: jsonHeaders, Body: `{"error":"malformed webhook body"}`}
	}

	// tombstoning needs only label/actor defaults; apply them.
	cfg.validate()
	handled := 0
	for _, a := range ev.Actions {
		if !isDeletionAction(a.Action) {
			continue
		}
		if err := tombstoneStory(cfg, a.ID, "webhook"); err != nil {
			// Item-level failures are logged and skipped; the webhook has
			// still consumed the event, and the in-window sweep is the
			// fallback (tk-thf acceptance).
			logInfo("webhook: tombstone story " + strconv.FormatInt(a.ID, 10) + " failed: " + truncateSnippet(err.Error(), 200))
			continue
		}
		handled++
	}
	logInfo("webhook: tombstoned " + strconv.Itoa(handled) + "/" + strconv.Itoa(len(ev.Actions)) + " deletion actions")
	return HTTPResponse{StatusCode: 204, Headers: jsonHeaders}
}

// isDeletionAction covers hard delete and archive (archived stories vanish
// from every active workspace query, so both need tombstones).
func isDeletionAction(action string) bool {
	return action == "story_delete" || action == "story_archive"
}

func (c *Config) verifyWebhookSignature(sig, body string) bool {
	if c.WebhookSecret == "" || sig == "" {
		return false
	}
	mac := hmac.New(sha256.New, []byte(c.WebhookSecret))
	mac.Write([]byte(body))
	want := hex.EncodeToString(mac.Sum(nil))
	// Constant-time compare against both the exact and case-folded value.
	return hmac.Equal([]byte(want), []byte(sig)) || hmac.Equal([]byte(want), []byte(lowerHex(sig)))
}

func lowerHex(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'F' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// tombstoneStory marks one story deleted: writes the KV marker and leaves a
// tombstone comment on the mapped item (if one exists) so operators can see
// the deletion lineage. The comment failure is non-fatal (kv marker is the
// source of truth); marker failures abort (kv layer is critical).
func tombstoneStory(cfg *Config, storyID int64, source string) error {
	marker := tombstoneMarker{DeletedAt: time.Now().UTC().Format(time.RFC3339), Source: source}
	if err := saveTombstone("story", storyID, marker); err != nil {
		return err
	}

	lk, err := itemLookup("story", storyID)
	if err != nil {
		if isRejected(err) {
			return nil // kv marker written; lookup rejection is item-level
		}
		return err
	}
	if !lk.Found {
		return nil // nothing mapped — nothing to annotate
	}
	itemID, err := itemIDInt(lk.ItemID)
	if err != nil {
		return nil
	}
	if _, err := createComment(createCommentRequest{
		ItemID:                itemID,
		AuthorID:              int(cfg.ActorUserID),
		Content:               fmt.Sprintf("[Shortcut] Story %d was deleted/archived in Shortcut — item tombstoned by the sync.", storyID),
		SuppressNotifications: true,
	}); err != nil {
		logInfo("tombstone comment failed for story " + strconv.FormatInt(storyID, 10) + ": " + truncateSnippet(err.Error(), 200))
	}
	return nil
}

// tombstoneMarker is the KV tombstone value (contract §5 shape).
type tombstoneMarker struct {
	DeletedAt string `json:"deleted_at"` // RFC3339
	Source    string `json:"source"`     // "webhook" | "sweep"
}

func saveTombstone(kind string, id int64, m tombstoneMarker) error {
	return kvSetString(tombstoneKey(kind, id), string(mustJSON(m)))
}

// --- operator control routes (tk-swm) ---

func handleGetConfig() HTTPResponse {
	cfg, err := loadConfig()
	if err != nil {
		return errorResponse(500, "config load failed")
	}
	if cfg == nil {
		return HTTPResponse{StatusCode: 200, Headers: jsonHeaders, Body: `{}`}
	}
	cfg.validate()
	rep := statusReport{}
	rep.Plugin = pluginName
	rep.Config.Enabled = cfg.Enabled
	rep.Config.DryRun = cfg.DryRun
	rep.Config.WorkspaceID = cfg.WorkspaceID
	rep.Config.ProjectIDs = cfg.ProjectIDs
	rep.Config.LabelMode = cfg.LabelMode
	rep.Config.ActorUserID = cfg.ActorUserID
	rep.Config.TokenSet = cfg.Token != ""
	rep.Config.WebhookSet = cfg.WebhookSecret != ""
	rep.Config.BackfillDays = cfg.BackfillDays
	st, err := loadState()
	if err != nil {
		return errorResponse(500, "state load failed")
	}
	rep.State = st
	body, _ := json.Marshal(rep)
	return HTTPResponse{StatusCode: 200, Headers: jsonHeaders, Body: string(body)}
}

// handleSaveConfig merges the update onto the stored config: nil token
// keeps the stored secret; explicit "" is rejected by validate.
func handleSaveConfig(body string) HTTPResponse {
	cfg, err := loadConfig()
	if err != nil {
		return errorResponse(500, "config load failed")
	}
	if cfg == nil {
		cfg = &Config{}
	}
	var up configUpdate
	if err := json.Unmarshal([]byte(body), &up); err != nil {
		return errorResponse(400, "malformed config JSON")
	}
	if up.Token != nil {
		cfg.Token = *up.Token
	}
	cfg.Enabled = up.Enabled
	cfg.DryRun = up.DryRun
	cfg.WorkspaceID = up.WorkspaceID
	cfg.ProjectIDs = up.ProjectIDs
	cfg.LabelMode = up.LabelMode
	cfg.ActorUserID = up.ActorUserID
	if up.WebhookSecret != nil {
		cfg.WebhookSecret = *up.WebhookSecret
	}
	cfg.BackfillDays = up.BackfillDays
	if err := cfg.validate(); err != nil {
		return errorResponse(400, err.Error())
	}
	if err := saveConfig(cfg); err != nil {
		return errorResponse(500, "config save failed")
	}
	logInfo("config updated via admin panel")
	return HTTPResponse{StatusCode: 204, Headers: jsonHeaders}
}

// handleSyncTick runs one engine tick inline (the "Tick now" button).
// The schedule owns nothing exclusive — run() is idempotent via cursors —
// but concurrent ticks would double-fire host calls, so the panel is the
// only manual entrypoint and operators are told not to spam it.
func handleSyncTick() HTTPResponse {
	cfg, err := loadConfig()
	if err != nil {
		return errorResponse(500, "config load failed")
	}
	if cfg == nil {
		return errorResponse(400, "not configured — save the connection form first")
	}
	if !cfg.Enabled {
		return errorResponse(400, "sync is disabled")
	}
	st, err := loadState()
	if err != nil {
		return errorResponse(500, "state corrupt (operator reset required)")
	}
	if st == nil {
		st = &State{Phase: phaseCatalog}
	}
	e := &engine{
		cfg:    cfg,
		st:     st,
		client: newShortcutClient(cfg),
		budget: logic.NewTickBudget(time.Now(), tickBudget),
		now:    now,
	}
	e.run()
	e.save()
	body, _ := json.Marshal(map[string]any{"counts": st.Counts, "phase": st.Phase, "errors": st.LastErrors})
	return HTTPResponse{StatusCode: 200, Headers: jsonHeaders, Body: string(body)}
}

// handleResetState clears the sync state (not the config). Items already
// created stay; the next tick rebuilds cursors from the watermark and
// item_lookup idempotency.
func handleResetState() HTTPResponse {
	if err := kvSetString(kvStateKey, ""); err != nil {
		return errorResponse(500, "state reset failed")
	}
	logInfo("sync state reset via admin panel")
	return HTTPResponse{StatusCode: 204, Headers: jsonHeaders}
}

func errorResponse(code int, msg string) HTTPResponse {
	body, _ := json.Marshal(map[string]string{"error": msg})
	return HTTPResponse{StatusCode: code, Headers: jsonHeaders, Body: string(body)}
}
