package bank

import (
	"testing"
	"time"
)

func TestIndexEvery(t *testing.T) {
	b := &Bank{}
	if _, err := b.Set("index.every", "15m"); err != nil || b.Every() != 15*time.Minute {
		t.Fatalf("every = %v, %v", b.Every(), err)
	}
	for _, bad := range []string{"10s", "soon"} {
		if _, err := b.Set("index.every", bad); err == nil {
			t.Errorf("%q was accepted", bad)
		}
	}
	if _, err := b.Set("index.every", "off"); err != nil || b.Every() != 0 {
		t.Fatalf("off: %v, %v", b.Every(), err)
	}
}
