package search_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/kgatilin/muninn/internal/search"
)

// What went wrong with the ranking opens the answer: the agent that reads it
// is the one who can tell the user.
func TestTheWarningOpensTheAnswer(t *testing.T) {
	var out bytes.Buffer
	search.Render{Warning: "the embedder could not be reached: token expired"}.Text(&out, "q", nil)
	if got := out.String(); !strings.HasPrefix(got, "! the embedder could not be reached: token expired\n! Tell the user") {
		t.Errorf("answer:\n%s", got)
	}
}
