// Package gcptest gives a test the Google Cloud surroundings of a run: a
// project, credentials on disk and a token endpoint that answers Token.
package gcptest

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/kgatilin/muninn/internal/gcp"
)

const (
	Project = "test-project"
	Token   = "ya29.test"
)

// Setup points the gcp package at a stub for the length of the test.
func Setup(t *testing.T) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil || r.Form.Get("refresh_token") != "refresh" || r.Form.Get("grant_type") != "refresh_token" {
			t.Errorf("token exchange: %v %v", err, r.Form)
		}
		io.WriteString(w, `{"access_token":"`+Token+`","expires_in":3600}`)
	}))
	t.Cleanup(srv.Close)
	creds := filepath.Join(t.TempDir(), "adc.json")
	if err := os.WriteFile(creds, []byte(`{"type":"authorized_user","client_id":"id","client_secret":"secret","refresh_token":"refresh"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GOOGLE_APPLICATION_CREDENTIALS", creds)
	t.Setenv(gcp.ProjectEnv, Project)
	t.Setenv(gcp.LocationEnv, "")
	old := gcp.TokenURL
	gcp.TokenURL = srv.URL
	t.Cleanup(func() { gcp.TokenURL = old })
}
