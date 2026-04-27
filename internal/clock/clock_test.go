package clock_test

import (
	"testing"
	"time"

	"github.com/Cidan/memmy/internal/clock"
)

func TestFakeClock(t *testing.T) {
	t0 := time.Date(2026, 4, 27, 12, 0, 0, 0, time.UTC)
	c := clock.NewFake(t0)
	if !c.Now().Equal(t0) {
		t.Fatalf("Now()=%v, want %v", c.Now(), t0)
	}

	c.Advance(time.Hour)
	if got, want := c.Now(), t0.Add(time.Hour); !got.Equal(want) {
		t.Fatalf("after Advance(1h): Now()=%v, want %v", got, want)
	}

	t1 := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	c.Set(t1)
	if !c.Now().Equal(t1) {
		t.Fatalf("after Set: Now()=%v, want %v", c.Now(), t1)
	}
}

func TestRealClock(t *testing.T) {
	c := clock.Real{}
	now := c.Now()
	if now.IsZero() {
		t.Fatal("Real clock returned zero time")
	}
}
