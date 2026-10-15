package eventbus

import (
	"testing"
	"time"
)

func TestParseBackoff(t *testing.T) {
	f, err := ParseBackoff("1s, 2s,500ms")
	if err != nil {
		t.Fatal(err)
	}
	for attempt, want := range map[int]time.Duration{-1: time.Second, 0: time.Second, 1: 2 * time.Second, 2: 500 * time.Millisecond, 9: 500 * time.Millisecond} {
		if got := f(attempt); got != want {
			t.Errorf("attempt %d: got %s, want %s", attempt, got, want)
		}
	}

	f, err = ParseBackoff("")
	if err != nil || f(3) != DefaultBackoff(3) {
		t.Errorf("empty: want DefaultBackoff, got err %v", err)
	}

	for _, bad := range []string{"1s,,2s", "fast", "0s", "-1s"} {
		if _, err := ParseBackoff(bad); err == nil {
			t.Errorf("%q: want error", bad)
		}
	}
}

func TestIdentityStreamsDoNotOverlap(t *testing.T) {
	deleted := IdentityDeletedStream().Subjects[0]
	for _, s := range IdentityStream().Subjects {
		if s == deleted || s == "yc.*.identity.>" || s == "yc.*.identity.*.*.*" {
			t.Errorf("YC_IDENTITY captures %s, which overlaps YC_IDENTITY_DELETED", s)
		}
	}
}
