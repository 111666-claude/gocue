package cue

import (
	"fmt"
	"time"
)

// TimeoutError reports that a probe or scan gave up because its deadline
// passed. The message is unchanged from the plain "timed out after …" text;
// the type lets callers (such as playlist planning) classify the failure.
type TimeoutError struct {
	After time.Duration
}

func (e TimeoutError) Error() string {
	return fmt.Sprintf("timed out after %s", e.After)
}

// ErrRequireAnalysis signals that existing tags are insufficient or stale and a
// full ffmpeg analysis is required. Callers should use errors.As to detect it
// and distinguish it from hard probe/scan failures.
type ErrRequireAnalysis struct {
	inner error
}

func (e ErrRequireAnalysis) Error() string {
	if e.inner == nil {
		return "not enough data, re-analysis is required"
	}
	return fmt.Sprintf("not enough data, re-analysis is required: %s", e.inner.Error())
}

// Unwrap exposes the underlying reason for errors.Is / errors.As chains.
func (e ErrRequireAnalysis) Unwrap() error {
	return e.inner
}
