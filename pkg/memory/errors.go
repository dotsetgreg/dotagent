package memory

import "errors"

var (
	// ErrContinuityUnavailable indicates prompt context could not be assembled
	// with enough prior state to answer safely for an existing conversation.
	ErrContinuityUnavailable = errors.New("memory continuity unavailable")
)
