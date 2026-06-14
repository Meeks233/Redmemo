package media

import (
	"fmt"
	"sync"
	"time"
)

type EventLevel string

const (
	LevelInfo  EventLevel = "info"
	LevelWarn  EventLevel = "warn"
	LevelError EventLevel = "error"
	LevelOK    EventLevel = "ok"
	LevelSkip  EventLevel = "skip"
)

type Event struct {
	Time    time.Time
	Level   EventLevel
	Phase   string
	Message string
}

func (e Event) DateStr() string {
	return e.Time.Format("2006-01-02")
}

func (e Event) ClockStr() string {
	return e.Time.Format("15:04:05 MST")
}

func (e Event) TimeStr() string {
	return e.Time.Format("2006-01-02 15:04:05 MST")
}

func (e Event) RelativeTime() string {
	d := time.Since(e.Time).Round(time.Second)
	if d < time.Minute {
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm%ds ago", int(d.Minutes()), int(d.Seconds())%60)
	}
	return fmt.Sprintf("%dh%dm ago", int(d.Hours()), int(d.Minutes())%60)
}

type EventLog struct {
	mu     sync.RWMutex
	events []Event
	cap    int
}

func NewEventLog(capacity int) *EventLog {
	return &EventLog{
		events: make([]Event, 0, capacity),
		cap:    capacity,
	}
}

func (l *EventLog) Add(level EventLevel, phase, msg string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cap <= 0 {
		return
	}
	e := Event{
		Time:    time.Now(),
		Level:   level,
		Phase:   phase,
		Message: msg,
	}
	if len(l.events) >= l.cap {
		copy(l.events, l.events[1:])
		l.events[len(l.events)-1] = e
	} else {
		l.events = append(l.events, e)
	}
}

func (l *EventLog) Addf(level EventLevel, phase, format string, args ...interface{}) {
	l.Add(level, phase, fmt.Sprintf(format, args...))
}

func (l *EventLog) Snapshot() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}
