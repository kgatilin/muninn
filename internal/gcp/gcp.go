// Package gcp is what the Vertex AI providers need of Google Cloud: the
// project and location of the run, and a bearer token from the application
// default credentials. Plain net/http, no SDKs.
//
// The credentials read are the ones `gcloud auth application-default login`
// writes: a user's refresh token. A service-account key file is not read.
package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	ProjectEnv      = "GOOGLE_CLOUD_PROJECT"
	LocationEnv     = "GOOGLE_CLOUD_LOCATION"
	credentialsEnv  = "GOOGLE_APPLICATION_CREDENTIALS"
	DefaultLocation = "global"

	defaultTokenURL = "https://oauth2.googleapis.com/token"
)

// TokenURL is a variable so that tests exchange the token with a stub.
var TokenURL = defaultTokenURL

type credentials struct {
	Type         string `json:"type"`
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	RefreshToken string `json:"refresh_token"`
	QuotaProject string `json:"quota_project_id"`
}

func credentialsPath() string {
	if p := os.Getenv(credentialsEnv); p != "" {
		return p
	}
	if dir := os.Getenv("CLOUDSDK_CONFIG"); dir != "" {
		return filepath.Join(dir, "application_default_credentials.json")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".config", "gcloud", "application_default_credentials.json")
}

func readCredentials() (credentials, error) {
	path := credentialsPath()
	raw, err := os.ReadFile(path)
	if err != nil {
		return credentials{}, fmt.Errorf("application default credentials: %w; run `gcloud auth application-default login`", err)
	}
	var c credentials
	if err := json.Unmarshal(raw, &c); err != nil {
		return credentials{}, fmt.Errorf("%s: %w", path, err)
	}
	if c.Type != "authorized_user" || c.RefreshToken == "" {
		return credentials{}, fmt.Errorf("%s holds credentials of type %q; only the authorized_user kind that `gcloud auth application-default login` writes is read", path, c.Type)
	}
	return c, nil
}

// Project is GOOGLE_CLOUD_PROJECT, else the quota project of the credentials.
func Project() (string, error) {
	if p := os.Getenv(ProjectEnv); p != "" {
		return p, nil
	}
	c, err := readCredentials()
	if err != nil {
		return "", err
	}
	if c.QuotaProject == "" {
		return "", fmt.Errorf("%s is not set and the application default credentials name no quota project", ProjectEnv)
	}
	return c.QuotaProject, nil
}

// Location is GOOGLE_CLOUD_LOCATION, else global.
func Location() string {
	if l := os.Getenv(LocationEnv); l != "" {
		return l
	}
	return DefaultLocation
}

// ModelURL is the address of a Google publisher model's method on Vertex AI.
// endpoint overrides the host, which otherwise follows the location.
func ModelURL(endpoint, model, method string) (string, error) {
	project, err := Project()
	if err != nil {
		return "", err
	}
	location := Location()
	host := strings.TrimRight(endpoint, "/")
	if host == "" {
		host = "https://aiplatform.googleapis.com"
		if location != "global" {
			host = "https://" + location + "-aiplatform.googleapis.com"
		}
	}
	return fmt.Sprintf("%s/v1/projects/%s/locations/%s/publishers/google/models/%s:%s", host, project, location, model, method), nil
}

var (
	mu      sync.Mutex
	token   string
	expires time.Time
)

var httpClient = &http.Client{Timeout: 30 * time.Second}

// Token is a bearer token for the Cloud APIs, exchanged from the refresh token
// and kept until a minute before it expires.
func Token(ctx context.Context) (string, error) {
	mu.Lock()
	defer mu.Unlock()
	if token != "" && time.Now().Before(expires) {
		return token, nil
	}
	c, err := readCredentials()
	if err != nil {
		return "", err
	}
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"client_id":     {c.ClientID},
		"client_secret": {c.ClientSecret},
		"refresh_token": {c.RefreshToken},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, TokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("google token exchange: %w", err)
	}
	defer resp.Body.Close()
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int    `json:"expires_in"`
		Error       string `json:"error"`
		Description string `json:"error_description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", fmt.Errorf("google token exchange: status %d: %w", resp.StatusCode, err)
	}
	if resp.StatusCode != http.StatusOK || out.AccessToken == "" {
		return "", fmt.Errorf("google token exchange: status %d: %s %s; run `gcloud auth application-default login`", resp.StatusCode, out.Error, out.Description)
	}
	token = out.AccessToken
	// Round(0) drops the monotonic reading: that clock stops while the machine
	// sleeps, and a token kept by it outlives itself by the length of the sleep.
	expires = time.Now().Add(time.Duration(out.ExpiresIn)*time.Second - time.Minute).Round(0)
	return token, nil
}
