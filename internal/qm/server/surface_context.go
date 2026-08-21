package server

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/loon-hejw/knowlega/internal/qm/auth"
	"github.com/loon-hejw/knowlega/internal/qm/data"
)

const (
	surfaceContextMaxMessages     = 200
	surfaceContextDefaultMessages = 100
	surfaceContextWait            = 25 * time.Second
	surfaceFileWait               = 120 * time.Second
	surfacePollInterval           = 100 * time.Millisecond
	surfacePendingWaitCap         = 20 * time.Second
	surfaceFileDownloadTTL        = 5 * time.Minute
)

var surfaceChannelIDPattern = regexp.MustCompile(`^[CG][A-Z0-9]{6,}$`)

func (h *HTTPServer) createSurfaceContext(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	if identity.Source || identity.ActorID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "capability_required", "message": "this endpoint is for the agent self-API"})
		return
	}
	body, ok := surfaceObjectBody(w, raw)
	if !ok {
		return
	}
	count := surfaceContextDefaultMessages
	if value, ok := body["count"].(float64); ok {
		count = int(math.Floor(value))
	}
	if count < 1 {
		count = 1
	}
	if count > surfaceContextMaxMessages {
		count = surfaceContextMaxMessages
	}
	query, source, channelName, ok := h.resolveSurfaceTarget(w, r, body, identity)
	if !ok {
		return
	}
	query["viewer"] = identity.ActorID
	query["count"] = count
	if before, ok := surfaceString(body["before"]); ok {
		query["before"] = before
	}
	if match, ok := surfaceString(body["match"]); ok {
		runes := []rune(match)
		if len(runes) > 200 {
			match = string(runes[:200])
		}
		query["match"] = match
	}
	request, err := h.createSurfaceRequest(r.Context(), source, query)
	if err != nil {
		h.fail(w, err)
		return
	}
	outcome, err := h.awaitSurfaceRequest(r.Context(), request.ID, surfaceContextWait)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		h.fail(w, err)
		return
	}
	if outcome == nil {
		if r.Context().Err() == nil {
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "surface_timeout", "message": "the surface didn't answer in time — try again"})
		}
		return
	}
	if outcome.Status == "failed" {
		message := "the surface couldn't answer that"
		if outcome.Error != nil {
			message = *outcome.Error
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "surface_error", "message": message})
		return
	}
	var result map[string]any
	if json.Unmarshal(outcome.Result, &result) != nil || result == nil {
		result = map[string]any{"messages": []any{}}
	}
	if channelName != "" {
		merged := map[string]any{"channel": "#" + channelName}
		for key, value := range result {
			merged[key] = value
		}
		result = merged
	}
	writeJSON(w, http.StatusOK, result)
}

func (h *HTTPServer) createSurfaceFile(w http.ResponseWriter, r *http.Request, raw []byte, identity auth.Identity) {
	if identity.Source || identity.ActorID == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "capability_required", "message": "this endpoint is for the agent self-API"})
		return
	}
	body, ok := surfaceObjectBody(w, raw)
	if !ok {
		return
	}
	ts, ok := surfaceString(body["ts"])
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "pass the message's `ts` (find it via /v1/surface-context)"})
		return
	}
	query, source, _, ok := h.resolveSurfaceTarget(w, r, body, identity)
	if !ok {
		return
	}
	file := map[string]any{"ts": ts}
	if threadTS, ok := surfaceString(body["threadTs"]); ok {
		file["threadTs"] = threadTS
	}
	if name, ok := surfaceString(body["name"]); ok {
		file["name"] = name
	}
	query["count"] = 1
	query["file"] = file
	request, err := h.createSurfaceRequest(r.Context(), source, query)
	if err != nil {
		h.fail(w, err)
		return
	}
	outcome, err := h.awaitSurfaceRequest(r.Context(), request.ID, surfaceFileWait)
	if err != nil {
		if r.Context().Err() != nil {
			return
		}
		h.fail(w, err)
		return
	}
	if outcome == nil {
		if r.Context().Err() == nil {
			writeJSON(w, http.StatusGatewayTimeout, map[string]string{"error": "surface_timeout", "message": "the surface didn't answer in time — try again"})
		}
		return
	}
	if outcome.Status == "failed" {
		message := "the surface couldn't answer that"
		if outcome.Error != nil {
			message = *outcome.Error
		}
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "surface_error", "message": message})
		return
	}
	var result struct {
		File *struct {
			BlobID    string `json:"blobId"`
			Name      string `json:"name"`
			SizeBytes any    `json:"sizeBytes"`
			Mimetype  string `json:"mimetype,omitempty"`
			Author    string `json:"author,omitempty"`
		} `json:"file"`
	}
	if json.Unmarshal(outcome.Result, &result) != nil || result.File == nil || result.File.BlobID == "" || result.File.Name == "" || result.File.SizeBytes == nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "surface_error", "message": "the surface can't fetch files yet (it may be mid-deploy) — tell the person plainly rather than retrying"})
		return
	}
	claims := auth.Claims{ActorID: identity.ActorID, ScopeID: identity.ScopeID, ScopeVersion: identity.ScopeVersion, Audience: "blob-transfer", Blob: &auth.BlobGrant{Dir: "read", ID: result.File.BlobID}, ExpiresAt: time.Now().Add(surfaceFileDownloadTTL).UnixMilli()}
	token, err := auth.MintCapability(claims, h.config.Auth.CapabilitySecret)
	if err != nil {
		h.fail(w, err)
		return
	}
	meta := map[string]any{"name": result.File.Name, "sizeBytes": result.File.SizeBytes}
	if result.File.Mimetype != "" {
		meta["mimetype"] = result.File.Mimetype
	}
	if result.File.Author != "" {
		meta["author"] = result.File.Author
	}
	path := "/v1/blobs/" + url.PathEscape(result.File.BlobID)
	writeJSON(w, http.StatusOK, map[string]any{
		"file":     meta,
		"download": map[string]any{"path": path, "header": auth.CapabilityHeader, "token": token, "expiresInSeconds": 300},
		"note":     `run: curl -fsS -H "` + auth.CapabilityHeader + `: <token>" "$AGENT_API_URL` + path + `" -o <path you choose — do NOT paste the posted filename into a shell unquoted>`,
	})
}

func (h *HTTPServer) pendingSurfaceContext(w http.ResponseWriter, r *http.Request) {
	source := "slack"
	if values, exists := r.URL.Query()["source"]; exists {
		source = ""
		if len(values) > 0 {
			source = values[0]
		}
	}
	wait := surfacePendingWait(r.URL.Query().Get("waitMs"))
	deadline := time.Now().Add(wait)
	for {
		requests, err := h.contextRequests.Pending(r.Context(), source, time.Now())
		if err != nil {
			h.fail(w, err)
			return
		}
		if len(requests) > 0 || !time.Now().Before(deadline) {
			output := make([]map[string]any, 0, len(requests))
			for _, request := range requests {
				var query map[string]any
				if err := json.Unmarshal(request.Query, &query); err != nil {
					h.fail(w, err)
					return
				}
				if request.ViewerTokenEncrypted != "" {
					token, err := h.connectorSecrets.Decrypt(request.ViewerTokenEncrypted)
					if err != nil {
						h.fail(w, err)
						return
					}
					query["viewerToken"] = token
				}
				output = append(output, map[string]any{"id": request.ID, "query": query})
			}
			writeJSON(w, http.StatusOK, map[string]any{"requests": output})
			return
		}
		timer := time.NewTimer(surfacePollInterval)
		select {
		case <-r.Context().Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (h *HTTPServer) createSurfaceRequest(ctx context.Context, source string, query map[string]any) (*data.ContextRequest, error) {
	raw, err := json.Marshal(query)
	if err != nil {
		return nil, err
	}
	return h.contextRequests.Create(ctx, source, raw, "")
}

func (h *HTTPServer) awaitSurfaceRequest(ctx context.Context, id string, wait time.Duration) (*data.ContextRequest, error) {
	cleanup := func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_, _ = h.contextRequests.Delete(cleanupCtx, id)
	}
	defer cleanup()
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		timer := time.NewTimer(surfacePollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
		request, err := h.contextRequests.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		if request == nil {
			return nil, nil
		}
		if request.Status == "done" || request.Status == "failed" {
			return request, nil
		}
	}
	return nil, nil
}

func (h *HTTPServer) resolveSurfaceTarget(w http.ResponseWriter, r *http.Request, body map[string]any, identity auth.Identity) (map[string]any, string, string, bool) {
	if channel, ok := surfaceString(body["channel"]); ok {
		ref := strings.TrimPrefix(strings.TrimSpace(channel), "#")
		if surfaceChannelIDPattern.MatchString(ref) {
			visible, err := h.surfaceChannelVisible(r.Context(), identity.ActorID, ref, nil)
			if err != nil {
				h.fail(w, err)
				return nil, "", "", false
			}
			if !visible {
				h.surfaceNotVisible(w, r, identity.ActorID, channel)
				return nil, "", "", false
			}
			return map[string]any{"channelId": ref}, "slack", "", true
		}
		matches, err := h.resolveSurfaceChannel(r.Context(), ref)
		if err != nil {
			h.fail(w, err)
			return nil, "", "", false
		}
		if len(matches) == 0 {
			writeJSON(w, http.StatusNotFound, map[string]string{"error": "channel_not_found", "message": `no channel I'm in matches "` + channel + `"`})
			return nil, "", "", false
		}
		if len(matches) > 1 {
			candidates := make([]map[string]string, 0, len(matches))
			for _, candidate := range matches {
				candidates = append(candidates, map[string]string{"channelId": candidate.ChannelID, "name": candidate.Name})
			}
			writeJSON(w, http.StatusConflict, map[string]any{"error": "ambiguous_channel", "message": `"` + channel + `" matches multiple channels — pass an exact name or id`, "candidates": candidates})
			return nil, "", "", false
		}
		candidate := matches[0]
		visible, err := h.surfaceChannelVisible(r.Context(), identity.ActorID, candidate.ChannelID, &candidate.IsPrivate)
		if err != nil {
			h.fail(w, err)
			return nil, "", "", false
		}
		if !visible {
			h.surfaceNotVisible(w, r, identity.ActorID, "#"+candidate.Name)
			return nil, "", "", false
		}
		return map[string]any{"channelId": candidate.ChannelID, "channelName": candidate.Name}, "slack", candidate.Name, true
	}
	if identity.Destination == nil || identity.Destination.Target == "" || identity.Destination.Type != "slack" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no_conversation", "message": "this conversation has no surface history to pull — name a channel instead"})
		return nil, "", "", false
	}
	return map[string]any{"conversationTarget": identity.Destination.Target}, identity.Destination.Type, "", true
}

func (h *HTTPServer) resolveSurfaceChannel(ctx context.Context, query string) ([]data.DirectoryChannel, error) {
	channels, err := h.directory.ListChannels(ctx)
	if err != nil {
		return nil, err
	}
	normalized := strings.ToLower(strings.TrimLeft(strings.TrimSpace(query), "@#"))
	if normalized == "" {
		return nil, nil
	}
	for _, channel := range channels {
		if strings.ToLower(channel.ChannelID) == normalized {
			return []data.DirectoryChannel{channel}, nil
		}
	}
	filter := func(match func(string) bool) []data.DirectoryChannel {
		result := make([]data.DirectoryChannel, 0, 10)
		for _, channel := range channels {
			name := strings.ToLower(strings.TrimLeft(strings.TrimSpace(channel.Name), "@#"))
			if match(name) {
				result = append(result, channel)
				if len(result) == 10 {
					break
				}
			}
		}
		return result
	}
	if exact := filter(func(name string) bool { return name == normalized }); len(exact) > 0 {
		return exact, nil
	}
	if prefix := filter(func(name string) bool { return strings.HasPrefix(name, normalized) }); len(prefix) > 0 {
		return prefix, nil
	}
	return filter(func(name string) bool { return strings.Contains(name, normalized) }), nil
}

func (h *HTTPServer) surfaceChannelVisible(ctx context.Context, actorID, channelID string, private *bool) (bool, error) {
	if private != nil {
		if !*private {
			return true, nil
		}
		return h.directory.IsScopeMember(ctx, "channel", channelID, actorID)
	}
	channels, err := h.directory.ChannelsFor(ctx, actorID)
	if err != nil {
		return false, err
	}
	for _, channel := range channels {
		if channel.ChannelID == channelID {
			return true, nil
		}
	}
	return false, nil
}

func (h *HTTPServer) surfaceNotVisible(w http.ResponseWriter, r *http.Request, actorID, label string) {
	member, err := h.directory.Get(r.Context(), actorID)
	if err != nil {
		h.fail(w, err)
		return
	}
	if member != nil {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "not_visible", "message": "I can only read a private channel you're in, and I can't confirm you're in " + label + "."})
		return
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "identity_unverified", "message": "I can't confirm your identity in this workspace — your login may not be linked to Slack — so I can't check whether you're in " + label + ". Connecting / signing in with Slack should fix it."})
}

func surfaceObjectBody(w http.ResponseWriter, raw []byte) (map[string]any, bool) {
	var decoded any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad_request", "message": "invalid JSON body"})
		return nil, false
	}
	body, ok := decoded.(map[string]any)
	if !ok {
		body = map[string]any{}
	}
	return body, true
}

func surfaceString(value any) (string, bool) {
	text, ok := value.(string)
	text = strings.TrimSpace(text)
	return text, ok && text != ""
}

func surfacePendingWait(value string) time.Duration {
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(number) || number < 0 {
		return 0
	}
	if number > float64(surfacePendingWaitCap/time.Millisecond) {
		number = float64(surfacePendingWaitCap / time.Millisecond)
	}
	return time.Duration(number * float64(time.Millisecond))
}
