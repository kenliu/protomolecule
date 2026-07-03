package main

import (
	"context"
	"time"
)

// Clock abstracts time for testing.
type Clock interface {
	Now() time.Time
}

// RealClock uses the real system clock.
type RealClock struct{}

func (RealClock) Now() time.Time { return time.Now() }

// FakeClock is a controllable clock for tests.
type FakeClock struct {
	T time.Time
}

func (f *FakeClock) Now() time.Time  { return f.T }
func (f *FakeClock) Set(t time.Time) { f.T = t }

// Executor runs a task and returns a result.
type Executor interface {
	Run(ctx context.Context, task TaskConfig, prompt string) (ExecutorResult, error)
}

// ExecutorResult captures the outcome of a single task execution.
type ExecutorResult struct {
	TaskID string
	CycleID    string
	Status     string // "success", "failed", "timeout", "fatal"
	StartedAt  time.Time
	FinishedAt time.Time
	Output     string
	Error      string
	Attempt    int
}
