package adapter

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"time"
)

type placeholderTokens struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
}

type placeholderAuthFile struct {
	AuthMode    string            `json:"auth_mode"`
	Tokens      placeholderTokens `json:"tokens"`
	LastRefresh time.Time         `json:"last_refresh"`
}

// placeholderAuth satisfies Codex's local JWT parsing without a usable bearer
// or refresh token. Only the gateway holds the real access token. Manual
// control-side refresh is required for this subscription-auth spike.
func placeholderAuth(accountID string, now time.Time) ([]byte, error) {
	claims := struct {
		Expires int64 `json:"exp"`
		Auth    struct {
			AccountID string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}{Expires: now.Add(365 * 24 * time.Hour).Unix()}
	claims.Auth.AccountID = accountID
	payload, err := json.Marshal(claims)
	if err != nil {
		return nil, fmt.Errorf("encode synthetic Codex claims: %w", err)
	}
	encode := base64.RawURLEncoding.EncodeToString
	jwt := encode([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + encode(payload) + "." + encode([]byte("inert-kagent-placeholder"))
	return json.Marshal(placeholderAuthFile{
		AuthMode: "chatgpt", LastRefresh: now,
		Tokens: placeholderTokens{IDToken: jwt, AccessToken: jwt, RefreshToken: "", AccountID: accountID},
	})
}
