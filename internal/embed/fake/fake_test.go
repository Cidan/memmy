package fake_test

import (
	"context"
	"testing"

	"github.com/Cidan/memmy/internal/embed/fake"
)

func TestFake_Deterministic(t *testing.T) {
	e := fake.New(64)
	ctx := context.Background()
	a, err := e.Embed(ctx, []string{"hello world"})
	if err != nil {
		t.Fatal(err)
	}
	b, err := e.Embed(ctx, []string{"hello world"})
	if err != nil {
		t.Fatal(err)
	}
	if len(a[0]) != 64 || len(b[0]) != 64 {
		t.Fatalf("dim mismatch: %d / %d, want 64", len(a[0]), len(b[0]))
	}
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			t.Fatalf("non-deterministic at %d: %v vs %v", i, a[0][i], b[0][i])
		}
	}
}

func TestFake_DistinguishesInputs(t *testing.T) {
	e := fake.New(64)
	a, _ := e.Embed(context.Background(), []string{"alpha"})
	b, _ := e.Embed(context.Background(), []string{"beta"})
	same := true
	for i := range a[0] {
		if a[0][i] != b[0][i] {
			same = false
			break
		}
	}
	if same {
		t.Fatal("different inputs produced identical vectors")
	}
}

func TestFake_NonZero(t *testing.T) {
	e := fake.New(128)
	v, _ := e.Embed(context.Background(), []string{"test"})
	allZero := true
	for _, x := range v[0] {
		if x != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Fatal("got all-zero vector")
	}
}

func TestFake_Dim(t *testing.T) {
	if got := fake.New(128).Dim(); got != 128 {
		t.Fatalf("Dim()=%d, want 128", got)
	}
	if got := fake.New(0).Dim(); got != 64 {
		t.Fatalf("Dim() for invalid input = %d, want 64 default", got)
	}
}
