package controller

import (
	"context"
	"sync"
	"time"

	"github.com/Realishbird/SR-Net-Speed-Test/internal/api"
)

// hub holds each agent's pending tasks for long-polling and routes the
// events and results that come back to whichever run is waiting on them.
type hub struct {
	mu       sync.Mutex
	queues   map[int64][]api.Task
	wake     map[int64]chan struct{}
	lastPoll map[int64]time.Time
	owner    map[string]int64      // task id -> agent id allowed to report on it
	subs     map[string]chan<- msg // task id -> run waiting on it
}

type msg struct {
	taskID string
	event  *api.Event
	result *api.LegResult
}

func newHub() *hub {
	return &hub{
		queues:   map[int64][]api.Task{},
		wake:     map[int64]chan struct{}{},
		lastPoll: map[int64]time.Time{},
		owner:    map[string]int64{},
		subs:     map[string]chan<- msg{},
	}
}

func (h *hub) wakeChan(agentID int64) chan struct{} {
	c, ok := h.wake[agentID]
	if !ok {
		c = make(chan struct{}, 1)
		h.wake[agentID] = c
	}
	return c
}

// send queues a task and, unless it is a cancel, routes its replies to sub.
func (h *hub) send(agentID int64, t api.Task, sub chan<- msg) {
	h.mu.Lock()
	h.queues[agentID] = append(h.queues[agentID], t)
	if sub != nil {
		h.owner[t.ID] = agentID
		h.subs[t.ID] = sub
	}
	c := h.wakeChan(agentID)
	h.mu.Unlock()
	select {
	case c <- struct{}{}:
	default:
	}
}

func (h *hub) forget(taskIDs ...string) {
	h.mu.Lock()
	for _, id := range taskIDs {
		delete(h.owner, id)
		delete(h.subs, id)
	}
	h.mu.Unlock()
}

// poll returns queued tasks, waiting up to wait for one to arrive.
func (h *hub) poll(ctx context.Context, agentID int64, wait time.Duration) []api.Task {
	deadline := time.NewTimer(wait)
	defer deadline.Stop()
	for {
		h.mu.Lock()
		h.lastPoll[agentID] = time.Now()
		tasks := h.queues[agentID]
		delete(h.queues, agentID)
		c := h.wakeChan(agentID)
		h.mu.Unlock()
		if len(tasks) > 0 {
			return tasks
		}
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return []api.Task{}
		case <-c:
		}
	}
}

// online means the agent is polling now or polled within the last 45 s.
func (h *hub) online(agentID int64) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return time.Since(h.lastPoll[agentID]) < 45*time.Second
}

// deliver routes an event or result to its run. Reports for unknown tasks, or
// from an agent that does not own the task, are dropped.
func (h *hub) deliver(agentID int64, m msg) bool {
	h.mu.Lock()
	sub, ok := h.subs[m.taskID]
	if ok && h.owner[m.taskID] != agentID {
		ok = false
	}
	h.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case sub <- m:
	case <-time.After(5 * time.Second):
		return false
	}
	return true
}
