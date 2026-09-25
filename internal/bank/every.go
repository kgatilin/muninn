package bank

import (
	"fmt"
	"time"
)

// Every is the bank's index.every: how often `muninn up` indexes it. A run
// over a bank with nothing new makes no paid call, so the period is what a
// walk of the bank's sources costs and nothing more.
func (b *Bank) Every() time.Duration {
	d, _ := time.ParseDuration(b.IndexEvery)
	return d
}

func everyKeys() []string {
	return []string{"index.every=<duration>|off (how often `muninn up` indexes the bank, as 15m)"}
}

func (b *Bank) setEvery(key, value string) (note string, err error) {
	if key != "index.every" {
		return "", fmt.Errorf("unknown key %q; accepted:\n  %s", key, everyKeys()[0])
	}
	if value == "off" {
		b.IndexEvery = ""
		return "", nil
	}
	if d, err := time.ParseDuration(value); err != nil || d < time.Minute {
		return "", fmt.Errorf("%s: a duration of a minute or more, as 15m, or off", key)
	}
	b.IndexEvery = value
	return "`muninn up` indexes the bank on this period; a running one picks the change up within a minute", nil
}
