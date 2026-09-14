package auth

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const CapabilityHeader = "x-agent-capability"

var localBlobID = regexp.MustCompile(`^[0-9a-f]{32}$`)

type Claims struct {
	ActorID         string                `json:"actorId"`
	ScopeID         string                `json:"scopeId"`
	Audience        string                `json:"aud,omitempty"`
	ScopeVersion    string                `json:"scopeVersion,omitempty"`
	LiveActor       bool                  `json:"liveActor,omitempty"`
	LiveAuthor      bool                  `json:"liveAuthor,omitempty"`
	Triggered       bool                  `json:"triggered,omitempty"`
	Blob            *BlobGrant            `json:"blob,omitempty"`
	Memory          *MemoryGrant          `json:"memory,omitempty"`
	KeychainMembers []KeychainMember      `json:"keychainMembers,omitempty"`
	Destination     *Destination          `json:"destination,omitempty"`
	Destinations    DestinationCandidates `json:"destinations,omitempty"`
	Drop            string                `json:"drop,omitempty"`
	ExpiresAt       int64                 `json:"exp"`
}

// MemoryGrant is the narrow per-conversation memory authority embedded in a
// control-plane capability. It intentionally mirrors Node's capability claim
// rather than turning the caller's primary scope into blanket memory access.
type MemoryGrant struct {
	Write    string   `json:"write,omitempty"`
	OrgWrite string   `json:"orgWrite,omitempty"`
	Read     []string `json:"read"`
}

// KeychainMember is the conversation participant projection carried in a
// capability. It grants an agent the narrowly-scoped ability to disconnect a
// participating member's OAuth connector, matching Node's keychainMembers
// claim without granting general keychain access.
type KeychainMember struct {
	ID string `json:"id"`
}

// DestinationCandidate is the deliberately narrow delivery destination
// projection carried by a control-plane capability. The control service may
// select only one of these signed candidates when retargeting a cron; it must
// never accept an arbitrary provider destination from the request body.
type DestinationCandidate struct {
	Key             string `json:"key"`
	Label           string `json:"label,omitempty"`
	Type            string `json:"type"`
	Target          string `json:"target"`
	AudienceScopeID string `json:"audienceScopeId,omitempty"`
}

type Destination struct {
	Type            string `json:"type"`
	Target          string `json:"target"`
	AudienceScopeID string `json:"audienceScopeId,omitempty"`
	OnBehalfOf      string `json:"onBehalfOf,omitempty"`
	EditRef         string `json:"editRef,omitempty"`
	UnfurlLinks     *bool  `json:"unfurlLinks,omitempty"`
	Identity        string `json:"identity,omitempty"`
	DebugFooter     string `json:"debugFooter,omitempty"`
}

// DestinationCandidates intentionally validates only the top-level array.
// That is the Node verifier's compatibility boundary: individual candidates
// are trusted, signed conversation projections and are selected by key later.
// Keeping malformed individual entries as empty candidates avoids rejecting a
// whole capability that Node would accept; such entries simply cannot match a
// requested non-empty key in the control route.
type DestinationCandidates []DestinationCandidate

func (c *DestinationCandidates) UnmarshalJSON(raw []byte) error {
	var items []json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return err
	}
	result := make([]DestinationCandidate, len(items))
	for i, item := range items {
		_ = json.Unmarshal(item, &result[i])
	}
	*c = result
	return nil
}

// BlobGrant is the deliberately narrow capability used by the raw Blob
// transfer endpoints. It mirrors Node's blob-transfer capability contract.
type BlobGrant struct {
	Dir string `json:"dir"`
	ID  string `json:"id,omitempty"`
}

type PortalIdentity struct {
	PrincipalID string `json:"p"`
	ExpiresAt   int64  `json:"exp"`
}

type Identity struct {
	ActorID         string
	ScopeID         string
	ScopeVersion    string
	Audience        string
	LiveActor       bool
	LiveAuthor      bool
	Triggered       bool
	Memory          *MemoryGrant
	KeychainMembers []KeychainMember
	Destination     *Destination
	Destinations    DestinationCandidates
	Source          bool
}

type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

func HTTPStatus(err error) int {
	var authError *Error
	if errors.As(err, &authError) {
		return authError.Status
	}
	return 401
}

func unauthorized(message string) error {
	return &Error{Status: http.StatusUnauthorized, Message: message}
}
func forbidden(message string) error { return &Error{Status: http.StatusForbidden, Message: message} }

type Verifier struct {
	SourceSecret     string
	CapabilitySecret string
	ReplayWindow     time.Duration
	DB               *pgxpool.Pool
	Now              func() time.Time
}

func (v Verifier) Authenticate(ctx context.Context, r *http.Request, body []byte, routeAuth string) (Identity, error) {
	if routeAuth == "public" {
		return Identity{}, nil
	}
	if token := r.Header.Get(CapabilityHeader); token != "" {
		claims, err := verifyCapability(token, v.CapabilitySecret, v.now())
		if err != nil {
			return Identity{}, err
		}
		if routeAuth == "source" {
			return Identity{}, forbidden("capability token not valid for this route")
		}
		if routeAuth != "either" && claims.Audience != routeAuth {
			return Identity{}, forbidden(fmt.Sprintf("this route requires a capability token with audience %q", routeAuth))
		}
		if routeAuth == "either" && claims.Audience != "" && claims.Audience != "control-plane" {
			return Identity{}, forbidden("capability token audience not valid for this route")
		}
		return Identity{ActorID: claims.ActorID, ScopeID: claims.ScopeID, ScopeVersion: claims.ScopeVersion, Audience: claims.Audience, LiveActor: claims.LiveActor, LiveAuthor: claims.LiveAuthor, Triggered: claims.Triggered, Memory: claims.Memory, KeychainMembers: claims.KeychainMembers, Destination: claims.Destination, Destinations: claims.Destinations}, nil
	}
	if routeAuth != "either" && routeAuth != "source" {
		return Identity{}, unauthorized("capability token required")
	}
	if err := v.verifySource(ctx, r, body); err != nil {
		return Identity{}, err
	}
	return Identity{Source: true}, nil
}

func (v Verifier) verifySource(ctx context.Context, r *http.Request, body []byte) error {
	canonical := r.Method + "\n" + r.URL.RequestURI() + "\n" + string(body)
	// Node treats source-authenticated reads as repeatable: they are signed for
	// integrity and freshness, but do not consume a replay key. Mutations keep
	// the durable replay guard so a retried delivery cannot be applied twice.
	return v.verifySourceCanonical(ctx, r, canonical, r.Method != http.MethodGet)
}

// VerifySourceCanonical verifies the source signature for endpoints whose
// canonical payload is intentionally different from the raw request body.
// Blob uploads use the declared SHA-256 so their bodies can remain streaming.
// Unlike ordinary source events, callers may disable replay persistence when
// the Node contract explicitly allows repeatable transfer attempts.
func (v Verifier) VerifySourceCanonical(ctx context.Context, r *http.Request, canonical string, deduplicate bool) error {
	return v.verifySourceCanonical(ctx, r, canonical, deduplicate)
}

func (v Verifier) verifySourceCanonical(ctx context.Context, r *http.Request, canonical string, deduplicate bool) error {
	if v.SourceSecret == "" {
		return unauthorized("source authentication is not configured")
	}
	timestamp, err := strconv.ParseInt(r.Header.Get("x-timestamp"), 10, 64)
	if err != nil {
		return unauthorized("invalid timestamp")
	}
	now := v.now()
	if delta := now.Sub(time.Unix(timestamp, 0)); delta > v.ReplayWindow || delta < -v.ReplayWindow {
		return unauthorized("stale timestamp (replay protection)")
	}
	mac := hmac.New(sha256.New, []byte(v.SourceSecret))
	_, _ = mac.Write([]byte("v0:" + strconv.FormatInt(timestamp, 10) + ":" + canonical))
	expected := "v0=" + fmt.Sprintf("%x", mac.Sum(nil))
	signature := r.Header.Get("x-signature")
	if signature == "" || !hmac.Equal([]byte(expected), []byte(signature)) {
		return unauthorized("signature mismatch")
	}
	if !deduplicate || v.DB == nil {
		return nil
	}
	expiresAt := time.Unix(timestamp, 0).Add(v.ReplayWindow)
	tag, err := v.DB.Exec(ctx, `INSERT INTO source_auth_replay(event_id, expires_at) VALUES($1,$2)
ON CONFLICT(event_id) DO UPDATE SET expires_at = EXCLUDED.expires_at WHERE source_auth_replay.expires_at < $3`, signature, expiresAt, now)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return unauthorized("duplicate event (already processed)")
	}
	_, _ = v.DB.Exec(ctx, "DELETE FROM source_auth_replay WHERE expires_at < $1", now)
	return nil
}

// VerifyBlobTransferCapability verifies the capability shape accepted by the
// Node raw transfer routes. Invalid blob-transfer grants are intentionally a
// forbidden response rather than the generic control-plane authentication
// response, matching the endpoint's public contract.
func (v Verifier) VerifyBlobTransferCapability(token, direction, blobID string) (Claims, error) {
	claims, err := verifyCapability(token, v.CapabilitySecret, v.now())
	if err != nil || claims.Audience != "blob-transfer" || claims.Blob == nil || claims.Blob.Dir != direction {
		return Claims{}, forbidden("blob-transfer capability token not valid for this transfer")
	}
	if direction == "write" {
		if claims.Blob.ID != "" {
			return Claims{}, forbidden("blob-transfer capability token not valid for this transfer")
		}
		return claims, nil
	}
	if direction != "read" || !localBlobID.MatchString(blobID) || claims.Blob.ID != blobID {
		return Claims{}, forbidden("blob-transfer capability token not valid for this transfer")
	}
	return claims, nil
}

func (v Verifier) now() time.Time {
	if v.Now != nil {
		return v.Now()
	}
	return time.Now()
}

func verifyCapability(token, secret string, now time.Time) (Claims, error) {
	if secret == "" {
		return Claims{}, unauthorized("capability authentication is not configured")
	}
	payload, err := verifySignedPayload(token, secret)
	if err != nil {
		return Claims{}, unauthorized("invalid or expired capability token")
	}
	var claims Claims
	var rawClaims map[string]json.RawMessage
	if err := json.Unmarshal(payload, &claims); err != nil || json.Unmarshal(payload, &rawClaims) != nil || claims.ActorID == "" || claims.ScopeID == "" || claims.ExpiresAt <= now.UnixMilli() || claims.Memory != nil && claims.Memory.Read == nil || rawClaimIsNull(rawClaims, "memory") || rawClaimIsNull(rawClaims, "keychainMembers") || rawClaimIsNull(rawClaims, "destinations") {
		return Claims{}, unauthorized("invalid or expired capability token")
	}
	return claims, nil
}

// Node distinguishes a missing optional claim from an explicit null: the
// latter is malformed for object/array claims. encoding/json maps both to nil
// pointers/slices, so retain the raw payload check to keep both verifiers on
// the same capability contract.
func rawClaimIsNull(claims map[string]json.RawMessage, key string) bool {
	raw, exists := claims[key]
	return exists && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}

func VerifyPortalIdentity(token, secret string, now time.Time) (*PortalIdentity, error) {
	if secret == "" || token == "" {
		return nil, unauthorized("portal identity required")
	}
	payload, err := verifySignedPayload(token, secret)
	if err != nil {
		return nil, unauthorized("portal identity required")
	}
	var identity PortalIdentity
	if err := json.Unmarshal(payload, &identity); err != nil || identity.PrincipalID == "" || identity.ExpiresAt < now.UnixMilli() {
		return nil, unauthorized("portal identity required")
	}
	return &identity, nil
}

// verifySignedPayload matches the Node signed-token helper.  New tokens are
// compact JWS values, while older deployments used base64url(JSON).HMAC.
func verifySignedPayload(token, secret string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) == 3 {
		header, err := base64.RawURLEncoding.DecodeString(parts[0])
		if err != nil {
			return nil, errors.New("invalid header")
		}
		var protected struct {
			Algorithm string `json:"alg"`
		}
		if err := json.Unmarshal(header, &protected); err != nil || protected.Algorithm != "HS256" {
			return nil, errors.New("invalid algorithm")
		}
		mac := hmac.New(sha256.New, []byte(secret))
		_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
		signature, err := base64.RawURLEncoding.DecodeString(parts[2])
		if err != nil || !hmac.Equal(mac.Sum(nil), signature) {
			return nil, errors.New("invalid signature")
		}
		return base64.RawURLEncoding.DecodeString(parts[1])
	}
	dot := strings.LastIndex(token, ".")
	if dot <= 0 {
		return nil, errors.New("invalid token")
	}
	payloadPart, signaturePart := token[:dot], token[dot+1:]
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(payloadPart))
	expected := mac.Sum(nil)
	signature, base64Err := base64.RawURLEncoding.DecodeString(signaturePart)
	if base64Err != nil || !hmac.Equal(expected, signature) {
		signature, hexErr := hex.DecodeString(signaturePart)
		if hexErr != nil || !hmac.Equal(expected, signature) {
			return nil, errors.New("invalid signature")
		}
	}
	return base64.RawURLEncoding.DecodeString(payloadPart)
}

func MintCapability(claims Claims, secret string) (string, error) {
	if secret == "" {
		return "", errors.New("capability secret not set")
	}
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	input := header + "." + base64.RawURLEncoding.EncodeToString(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	_, _ = mac.Write([]byte(input))
	return input + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}
