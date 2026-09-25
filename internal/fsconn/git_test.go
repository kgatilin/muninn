package fsconn

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestGitAdded(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	root := t.TempDir()
	git := func(date string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", root, "-c", "user.name=t", "-c", "user.email=t@example.com"}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+date, "GIT_COMMITTER_DATE="+date)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("", "init", "-q")
	os.MkdirAll(filepath.Join(root, "docs"), 0o755)
	os.WriteFile(filepath.Join(root, "docs", "old.md"), []byte("old"), 0o644)
	git("2025-01-10T12:00:00Z", "add", ".")
	git("2025-01-10T12:00:00Z", "commit", "-qm", "old")
	os.WriteFile(filepath.Join(root, "docs", "new.md"), []byte("new"), 0o644)
	os.WriteFile(filepath.Join(root, "docs", "old.md"), []byte("old, edited"), 0o644)
	git("2025-06-01T12:00:00Z", "add", ".")
	git("2025-06-01T12:00:00Z", "commit", "-qm", "new")

	added := gitAdded(filepath.Join(root, "docs"))
	for path, want := range map[string]string{"old.md": "2025-01-10T12:00:00Z", "new.md": "2025-06-01T12:00:00Z"} {
		w, _ := time.Parse(time.RFC3339, want)
		if !added[path].Equal(w) {
			t.Errorf("%s added %v, want %v", path, added[path], w)
		}
	}
	if len(gitAdded(t.TempDir())) != 0 {
		t.Error("a directory outside git has times")
	}
}
