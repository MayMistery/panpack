package credentials

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Credentials struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AppKey       string `json:"app_key,omitempty"`
	SecretKey    string `json:"secret_key,omitempty"`
	ExpiresAt    int64  `json:"expires_at,omitempty"`
	ExpiresIn    int64  `json:"expires_in,omitempty"` // accepted for bypy compatibility
	Scope        string `json:"scope,omitempty"`
}

func Discover(explicit string) (Credentials, string, error) {
	if token := os.Getenv("PANPACK_ACCESS_TOKEN"); token != "" {
		return Credentials{AccessToken: token}, "PANPACK_ACCESS_TOKEN", nil
	}
	var candidates []string
	if explicit != "" {
		candidates = append(candidates, explicit)
	}
	if envPath := os.Getenv("PANPACK_TOKEN_FILE"); envPath != "" {
		candidates = append(candidates, envPath)
	}
	home, _ := os.UserHomeDir()
	if home != "" {
		candidates = append(candidates,
			filepath.Join(home, ".config", "panpack", "credentials.json"),
			filepath.Join(home, ".bypy", "bypy.json"),
		)
	}
	seen := map[string]struct{}{}
	for _, path := range candidates {
		path = filepath.Clean(path)
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		creds, err := Load(path)
		if err == nil {
			return creds, path, nil
		}
		if !os.IsNotExist(err) {
			return Credentials{}, "", err
		}
	}
	return Credentials{}, "", errors.New("no credentials found; run 'panpack auth login' or set PANPACK_ACCESS_TOKEN")
}

func Load(path string) (Credentials, error) {
	creds, err := LoadUnchecked(path)
	if err != nil {
		return Credentials{}, err
	}
	if creds.Expired(time.Now()) {
		return Credentials{}, fmt.Errorf("credentials %s have expired", path)
	}
	return creds, nil
}

func LoadUnchecked(path string) (Credentials, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Credentials{}, err
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		return Credentials{}, fmt.Errorf("decode credentials %s: %w", path, err)
	}
	if creds.AccessToken == "" {
		return Credentials{}, fmt.Errorf("credentials %s contain no access_token", path)
	}
	return creds, nil
}

func (c Credentials) Expired(now time.Time) bool {
	expiresAt := c.ExpiresAt
	// Some bypy versions persist expires_in as an absolute Unix timestamp.
	if expiresAt == 0 && c.ExpiresIn > 1_000_000_000 {
		expiresAt = c.ExpiresIn
	}
	return expiresAt > 0 && now.Unix() >= expiresAt-60
}

func Save(path string, creds Credentials) error {
	if creds.AccessToken == "" {
		return errors.New("refusing to save empty access token")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(creds, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

func DefaultPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "panpack", "credentials.json"), nil
}
