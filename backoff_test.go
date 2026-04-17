package sftp

import (
	"testing"
	"time"
)

func TestBackoffResetAndCount(t *testing.T) {
	b := &backoff{}
	if b.count() != 0 {
		t.Errorf("initial count = %d, want 0", b.count())
	}
	b.attempt = 3
	if b.count() != 3 {
		t.Errorf("count = %d, want 3", b.count())
	}
	b.reset()
	if b.count() != 0 {
		t.Errorf("count after reset = %d, want 0", b.count())
	}
}

func TestBackoffCustomHandler(t *testing.T) {
	var calledWith []int
	handler := func(attempt int) time.Duration {
		calledWith = append(calledWith, attempt)
		return 0 // no actual sleep in test
	}
	b := &backoff{handler: handler}
	b.backoff()
	b.backoff()
	b.backoff()

	if b.count() != 3 {
		t.Errorf("count = %d, want 3", b.count())
	}
	if len(calledWith) != 3 {
		t.Fatalf("handler called %d times, want 3", len(calledWith))
	}
	for i, v := range calledWith {
		if v != i {
			t.Errorf("calledWith[%d] = %d, want %d", i, v, i)
		}
	}
}
