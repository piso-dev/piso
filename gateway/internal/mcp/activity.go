package mcp

import (
	"encoding/json"
	"net/http"
	"strings"

	"piso/gateway/internal/store"
)

func (h *Handler) registerBoard() {
	h.Register(Tool{
		Name:        "activity_post",
		Description: "Post a board event: progress, milestone, reminder, poke, active, waiting, host, or note.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind":       map[string]any{"type": "string"},
				"text":       map[string]any{"type": "string"},
				"targetSlug": map[string]any{"type": "string"},
			},
			"required": []string{"kind", "text"},
		},
		Write: true,
		Call:  h.toolActivityPost,
	})
	h.Register(Tool{
		Name:        "activity_feed",
		Description: "This worker's own board rows.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"kind":  map[string]any{"type": "string"},
				"since": map[string]any{"type": "integer"},
			},
		},
		Call: h.toolActivityFeed,
	})
	h.Register(Tool{
		Name:        "host_request",
		Description: "Ask a person to run a script on the host. Supply explanation (why, effects, prerequisites, working directory and run instructions) and script (inline contents or existing filename); resultPath names the output. Legacy command is still accepted. Files a tracked, blocking request AND a board line. With taskId, requires attemptId, requestId, summary, checkpoint; atomically waits and ends execution. Exit after success.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"command":     map[string]any{"type": "string", "description": "Legacy command; prefer script plus explanation."},
				"explanation": map[string]any{"type": "string"},
				"script":      hostScriptSchema(),
				"resultPath":  map[string]any{"type": "string"},
				"title":       map[string]any{"type": "string"},
				"category":    map[string]any{"type": "string"},
				"taskId":      map[string]any{"type": "string"},
				"attemptId":   map[string]any{"type": "string"},
				"requestId":   map[string]any{"type": "string"},
				"summary":     map[string]any{"type": "string"},
				"checkpoint":  map[string]any{"type": "string"},
			},
			"required": []string{"resultPath"},
			"oneOf": []any{
				map[string]any{"required": []string{"script", "explanation"}},
				map[string]any{"required": []string{"command"}},
			},
		},
		Write: true,
		Call:  h.toolHostRequest,
	})
}

func agentActivityKind(k string) bool {
	switch k {
	case store.ActivityKindProgress, store.ActivityKindMilestone, store.ActivityKindReminder,
		store.ActivityKindPoke, store.ActivityKindActive, store.ActivityKindWaiting,
		store.ActivityKindHost, store.ActivityKindNote:
		return true
	}
	return false
}

func (h *Handler) insertBoard(caller store.WorkerRec, kind, text, target string) (any, *rpcError) {
	return h.insertBoardAsk(caller, kind, text, target, "")
}

func (h *Handler) insertBoardAsk(caller store.WorkerRec, kind, text, target, askID string) (any, *rpcError) {
	if h.Store == nil {
		return nil, &rpcError{Code: codeInternal, Message: "activity store unavailable"}
	}
	kind = strings.TrimSpace(kind)
	text = store.SanitizeLabel(text, 500)
	target = store.SanitizeLabel(target, 63)
	if !agentActivityKind(kind) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown kind"}
	}
	if text == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "text required"}
	}
	stored, err := h.Store.InsertActivity(store.Activity{
		Worker:     caller.Name,
		Slug:       caller.Slug,
		Kind:       kind,
		TargetSlug: target,
		Text:       text,
		AskID:      askID,
	})
	if err != nil {
		return nil, &rpcError{Code: codeServer, Message: err.Error()}
	}
	return map[string]any{"id": stored.ID, "ts": stored.Ts, "kind": stored.Kind}, nil
}

func (h *Handler) toolActivityPost(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	var in struct {
		Kind       string `json:"kind"`
		Text       string `json:"text"`
		TargetSlug string `json:"targetSlug"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "kind and text required"}
	}
	return h.insertBoard(caller, in.Kind, in.Text, in.TargetSlug)
}

// toolHostRequest is "I cannot continue until a person does this", not "FYI".
//
// It used to write one activity row and return, which meant the worker exited
// with no record a human could answer and nothing holding the task. Now the
// durable HumanAction is written first (and, with taskId, the wait that ends
// the lease), and the activity row is the timeline trace pointing at it.
//
// Order is load-bearing. The ask is the block; the activity is the trace. A
// failed activity write must not lose the block, so it degrades to a warning —
// but a failed ask with taskId must not fall back to the activity alone, or a
// worker would exit believing it is blocked while nothing holds the task.
func (h *Handler) toolHostRequest(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	var in struct {
		Command     string      `json:"command"`
		Explanation string      `json:"explanation"`
		Script      *HostScript `json:"script"`
		ResultPath  string      `json:"resultPath"`
		Title       string      `json:"title"`
		Category    string      `json:"category"`
		TaskID      string      `json:"taskId"`
		AttemptID   string      `json:"attemptId"`
		RequestID   string      `json:"requestId"`
		Summary     string      `json:"summary"`
		Checkpoint  string      `json:"checkpoint"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "command and resultPath required"}
	}
	// Keep the command verbatim (only trimmed) for the ask: a human copies it
	// into a shell, so collapsing its whitespace would change what runs. The
	// board line gets a sanitized copy like every other activity row.
	command := strings.TrimSpace(in.Command)
	resultPath := strings.TrimSpace(in.ResultPath)
	if in.Script != nil {
		if command != "" || !in.Script.valid() || !scriptTextValid(in.Explanation, 20000) {
			return nil, &rpcError{Code: codeInvalidParams, Message: "valid script and explanation required; do not combine script with command"}
		}
		command = in.Script.Filename
		if in.Script.Kind == "file" {
			command = in.Script.Path
		}
	} else if !scriptTextValid(command, 20000) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "command required (maximum 20000 UTF-8 bytes)"}
	}
	if resultPath == "" {
		return nil, &rpcError{Code: codeInvalidParams, Message: "resultPath required"}
	}
	category := strings.TrimSpace(in.Category)
	if category == "" {
		category = "host"
	}
	taskLinked := strings.TrimSpace(in.TaskID) != ""

	body := map[string]any{
		"category":     category,
		"instructions": command,
		"verification": store.SanitizeLabel(resultPath, 2000),
	}
	if in.Script != nil {
		body["script"] = in.Script
		body["explanation"] = in.Explanation
	}
	if t := strings.TrimSpace(in.Title); t != "" {
		body["title"] = store.SanitizeLabel(t, 300)
	} else {
		body["title"] = "Host: " + store.SanitizeLabel(command, 292)
	}
	if taskLinked {
		body["taskId"] = strings.TrimSpace(in.TaskID)
		body["attemptId"] = strings.TrimSpace(in.AttemptID)
		body["requestId"] = strings.TrimSpace(in.RequestID)
		body["summary"] = in.Summary
		body["checkpoint"] = in.Checkpoint
	} else if id := strings.TrimSpace(in.RequestID); id != "" {
		// The untasked form dedupes on requestId at the coordinator; without
		// one, a retry is a second ask, which is the pre-existing behaviour.
		body["requestId"] = id
	}
	if rpcErr := requireTaskAskFields(body); rpcErr != nil {
		return nil, rpcErr
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return nil, &rpcError{Code: codeInvalidParams, Message: "invalid request"}
	}

	var askID, waitID string
	var warning string
	status, doc, rpcErr := h.coord(caller, http.MethodPost, "/api/v1/human-actions", nil, payload)
	switch {
	case rpcErr != nil:
		// Coordinator unreachable, or this worker is not enrolled. A block
		// that only exists on the board is worse than a loud failure.
		if taskLinked {
			return nil, rpcErr
		}
		warning = "not tracked (" + rpcErr.Message + "): recorded on the board only"
	case status == 0 || doc["status"] == "unpaired":
		if taskLinked {
			return nil, &rpcError{Code: codeServer, Message: "coordinator unpaired: cannot file a blocking host request"}
		}
		warning = "not tracked (coordinator unpaired): recorded on the board only"
	case status >= 400:
		return nil, coordErr(status, doc)
	default:
		// A task-linked ask answers with the task's own id plus humanActionId
		// (mutate_worker's response shape); the untasked create answers with
		// the ask's id. Prefer the explicit field so the timeline row points at
		// the ask and not at the task.
		askID, _ = doc["humanActionId"].(string)
		if askID == "" {
			askID, _ = doc["id"].(string)
		}
		waitID, _ = doc["waitId"].(string)
	}

	// Budget for insertBoardAsk's 500-char cap: command + " · result → " + path +
	// " · ask <36-char id>" has to fit, or the ask link is what gets truncated.
	text := store.SanitizeLabel(command, 240) + " · result → " + store.SanitizeLabel(resultPath, 120)
	if askID != "" {
		text += " · ask " + askID
	}
	board, boardErr := h.insertBoardAsk(caller, store.ActivityKindHost, text, "", askID)
	if boardErr != nil {
		if taskLinked || askID != "" {
			// The ask landed (and the lease ended); losing the timeline trace
			// must not turn that into a failure the worker retries.
			warning = "board line failed: " + boardErr.Message
		} else {
			return nil, boardErr
		}
	}

	out := map[string]any{"kind": store.ActivityKindHost, "command": command, "resultPath": resultPath}
	if askID != "" {
		out["humanActionId"] = askID
		out["status"] = "requested"
	} else {
		out["status"] = "untracked"
	}
	if waitID != "" {
		out["waitId"] = waitID
	}
	if taskLinked {
		// The coordinator already echoed it, but taking it from the request
		// keeps the response honest even on a replayed receipt.
		out["taskId"] = strings.TrimSpace(in.TaskID)
	}
	if v, ok := board.(map[string]any); ok {
		out["activityId"] = v["id"]
		out["ts"] = v["ts"]
	}
	if warning != "" {
		out["warning"] = warning
	}
	return out, nil
}

func (h *Handler) toolActivityFeed(caller store.WorkerRec, args json.RawMessage) (any, *rpcError) {
	if err := rejectWorkerArg(args); err != nil {
		return nil, err
	}
	if h.Store == nil {
		return nil, &rpcError{Code: codeInternal, Message: "activity store unavailable"}
	}
	var in struct {
		Kind  string `json:"kind"`
		Since int64  `json:"since"`
	}
	_ = json.Unmarshal(args, &in)
	f := store.ActivityFilter{
		Slug:    caller.Slug,
		Kind:    strings.TrimSpace(in.Kind),
		SinceMs: in.Since,
		Limit:   50,
	}
	if f.Kind != "" && !store.ValidActivityKind(f.Kind) {
		return nil, &rpcError{Code: codeInvalidParams, Message: "unknown kind"}
	}
	rows, err := h.Store.QueryActivities(f)
	if err != nil {
		return nil, &rpcError{Code: codeServer, Message: err.Error()}
	}
	now := h.Now().UnixNano() / 1000000
	out := make([]map[string]any, 0, len(rows))
	for _, a := range rows {
		age := int64(0)
		if now > a.Ts {
			age = (now - a.Ts) / 60000
		}
		row := map[string]any{
			"id":     a.ID,
			"kind":   a.Kind,
			"text":   a.Text,
			"ts":     a.Ts,
			"ageMin": age,
		}
		if a.TargetSlug != "" {
			row["targetSlug"] = a.TargetSlug
		}
		out = append(out, row)
	}
	return map[string]any{"status": "ok", "rows": out}, nil
}
