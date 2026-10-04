package chatgptrefresh

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

const AuthKey = "auth.json"
const StateAnnotation = "kagent.dev/chatgpt-refresh-state"
const ClaimAnnotation = "kagent.dev/chatgpt-refresh-claim"
const HashAnnotation = "kagent.dev/chatgpt-refresh-auth-hash"
const RetryAnnotation = "kagent.dev/chatgpt-refresh-retry-after"
const ReauthenticationRequired = "ReauthenticationRequired"
const statePending = "pending"
const stateRunning = "running"
const stateRejected = "reauthentication-required"
const safetyMargin = 10 * time.Minute
const codexVersion = "0.148.0"

var errCredential = errors.New("invalid dedicated ChatGPT credential")
var errOperation = errors.New("ChatGPT credential operation failed")
var errReauthentication = errors.New("ChatGPT re-authentication required")

type authFile struct {
	AuthMode string  `json:"auth_mode"`
	APIKey   *string `json:"OPENAI_API_KEY"`
	Tokens   struct {
		Access    string `json:"access_token"`
		Refresh   string `json:"refresh_token"`
		AccountID string `json:"account_id"`
	} `json:"tokens"`
}

// Decode only scheduling and validation metadata. Persist the original bytes,
// including Codex-owned fields this controller does not understand.
func readAuth(data []byte) (authFile, error) {
	var auth authFile
	if len(data) > 256*1024 || json.Unmarshal(data, &auth) != nil ||
		(auth.AuthMode != "" && auth.AuthMode != "chatgpt") || auth.APIKey != nil ||
		auth.Tokens.Access == "" || auth.Tokens.Refresh == "" || auth.Tokens.AccountID == "" {
		return authFile{}, errCredential
	}
	if _, err := expiry([]byte(auth.Tokens.Access)); err != nil {
		return authFile{}, err
	}
	return auth, nil
}

func expiry(token []byte) (time.Time, error) {
	parts := strings.Split(string(token), ".")
	if len(parts) != 3 || len(token) > 64*1024 {
		return time.Time{}, errCredential
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, errCredential
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 || claims.Exp > 253402300799 {
		return time.Time{}, errCredential
	}
	return time.Unix(claims.Exp, 0), nil
}

func due(token []byte, now time.Time) (bool, error) {
	expires, err := expiry(token)
	return err == nil && !now.Before(expires.Add(-safetyMargin)), err
}

func authHash(data []byte) string {
	hash := sha256.Sum256(data)
	return hex.EncodeToString(hash[:])
}

func rotatedAuth(before, after []byte, now time.Time) (authFile, error) {
	old, err := readAuth(before)
	if err != nil {
		return authFile{}, err
	}
	current, err := readAuth(after)
	if err != nil {
		return authFile{}, err
	}
	expires, _ := expiry([]byte(current.Tokens.Access))
	if old.Tokens.Refresh == current.Tokens.Refresh || old.Tokens.Access == current.Tokens.Access ||
		old.Tokens.AccountID != current.Tokens.AccountID || !expires.After(now.Add(safetyMargin)) {
		return authFile{}, errReauthentication
	}
	return current, nil
}
