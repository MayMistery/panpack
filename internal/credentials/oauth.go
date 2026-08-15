package credentials

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/baidu-netdisk/baidu-drive-sdk-go/baidudriver/api"
)

func DeviceLogin(ctx context.Context, appKey, secretKey string, out io.Writer) (Credentials, error) {
	if appKey == "" || secretKey == "" {
		return Credentials{}, errors.New("app key and secret key are required")
	}
	client := api.NewClient()
	device, err := client.Auth.DeviceCode(ctx, appKey)
	if err != nil {
		return Credentials{}, fmt.Errorf("request device code: %w", err)
	}
	fmt.Fprintf(out, "Open: %s\nCode: %s\n", device.VerificationURL, device.UserCode)
	if device.QrcodeURL != "" {
		fmt.Fprintf(out, "QR:   %s\n", device.QrcodeURL)
	}

	interval := time.Duration(device.Interval) * time.Second
	if interval < time.Second {
		interval = 5 * time.Second
	}
	deadline := time.NewTimer(time.Duration(device.ExpiresIn) * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return Credentials{}, ctx.Err()
		case <-deadline.C:
			return Credentials{}, errors.New("device authorization expired")
		case <-ticker.C:
			token, err := client.Auth.DeviceToken(ctx, appKey, secretKey, device.DeviceCode)
			if err != nil {
				if isPending(err) {
					continue
				}
				return Credentials{}, fmt.Errorf("exchange device token: %w", err)
			}
			return Credentials{
				AccessToken:  token.AccessToken,
				RefreshToken: token.RefreshToken,
				AppKey:       appKey,
				SecretKey:    secretKey,
				ExpiresAt:    time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).Unix(),
				Scope:        token.Scope,
			}, nil
		}
	}
}

func Refresh(ctx context.Context, creds Credentials) (Credentials, error) {
	if creds.RefreshToken == "" || creds.AppKey == "" || creds.SecretKey == "" {
		return Credentials{}, errors.New("refresh token, app key, and secret key are required")
	}
	q := url.Values{}
	q.Set("grant_type", "refresh_token")
	q.Set("refresh_token", creds.RefreshToken)
	q.Set("client_id", creds.AppKey)
	q.Set("client_secret", creds.SecretKey)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://openapi.baidu.com/oauth/2.0/token?"+q.Encode(), nil)
	if err != nil {
		return Credentials{}, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Credentials{}, err
	}
	defer resp.Body.Close()
	var token struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		ExpiresIn        int    `json:"expires_in"`
		Scope            string `json:"scope"`
		Error            string `json:"error"`
		ErrorDescription string `json:"error_description"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&token); err != nil {
		return Credentials{}, err
	}
	if token.Error != "" {
		return Credentials{}, fmt.Errorf("oauth refresh: %s: %s", token.Error, token.ErrorDescription)
	}
	if token.AccessToken == "" {
		return Credentials{}, errors.New("oauth refresh returned no access token")
	}
	creds.AccessToken = token.AccessToken
	creds.RefreshToken = token.RefreshToken
	creds.ExpiresAt = time.Now().Add(time.Duration(token.ExpiresIn) * time.Second).Unix()
	creds.ExpiresIn = 0
	creds.Scope = token.Scope
	return creds, nil
}

func isPending(err error) bool {
	var apiErr *api.APIError
	if errors.As(err, &apiErr) {
		msg := strings.ToLower(apiErr.Errmsg + " " + apiErr.ResponseBody)
		return strings.Contains(msg, "authorization_pending") || strings.Contains(msg, "slow_down")
	}
	return false
}
