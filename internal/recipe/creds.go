package recipe

import "encoding/json"

// Credentials is the plaintext secret payload for a login recipe. It is the ONLY
// carrier of credential VALUES and lives exclusively inside the encrypted blob
// (never in Step, never persisted or logged in the clear). Extra allows future
// named secrets beyond username/password without a schema change.
type Credentials struct {
	Username string            `json:"username,omitempty"`
	Password string            `json:"password,omitempty"`
	Extra    map[string]string `json:"extra,omitempty"`
}

// Marshal encodes the credentials to JSON (the plaintext that gets encrypted).
func (c Credentials) Marshal() ([]byte, error) { return json.Marshal(c) }

// ParseCredentials decodes the decrypted JSON blob.
func ParseCredentials(b []byte) (Credentials, error) {
	var c Credentials
	if err := json.Unmarshal(b, &c); err != nil {
		return Credentials{}, err
	}
	return c, nil
}

// Map returns the ref→value lookup the crawler uses to substitute fill steps.
func (c Credentials) Map() map[string]string {
	m := map[string]string{}
	if c.Username != "" {
		m[RefUsername] = c.Username
	}
	if c.Password != "" {
		m[RefPassword] = c.Password
	}
	for k, v := range c.Extra {
		m[k] = v
	}
	return m
}
