package sdk

import "time"

// Token is an OAuth2 access token with an optional refresh token and expiry.
type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token,omitempty"`
	Expiry       time.Time `json:"expiry,omitempty"`
}

// IsExpired reports whether the token expires within a minute, so callers refresh
// before sending a request with a dying token. A zero Expiry never expires.
func (t *Token) IsExpired() bool {
	if t.Expiry.IsZero() {
		return false
	}
	return time.Now().Add(1 * time.Minute).After(t.Expiry)
}

// Credentials is the secret for one connector instance: a Token or an APIKey.
type Credentials struct {
	Token  *Token
	APIKey string
}
