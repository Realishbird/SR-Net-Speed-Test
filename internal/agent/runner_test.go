package agent

import (
	"testing"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

func TestTaskTimeout(t *testing.T) {
	now := time.Date(2026, 10, 21, 16, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		task api.Task
		want time.Duration
	}{
		{"server without start time", api.Task{Kind: api.TaskServer, DurationSec: 60}, 3 * time.Minute},
		{"client waiting 3 s", api.Task{Kind: api.TaskClient, DurationSec: 60, StartAt: now.Add(3 * time.Second)}, 3*time.Minute + 3*time.Second},
		{"client whose start time passed", api.Task{Kind: api.TaskClient, DurationSec: 60, StartAt: now.Add(-time.Second)}, 3 * time.Minute},
	}
	for _, c := range cases {
		if got := taskTimeout(c.task, now); got != c.want {
			t.Errorf("%s: got %s, want %s", c.name, got, c.want)
		}
	}
}
