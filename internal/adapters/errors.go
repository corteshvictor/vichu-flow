package adapters

import "errors"

// ErrResumeUnsupported is returned by adapters whose backing tool cannot resume
// a prior session. The engine falls back to a fresh invocation with context.
var ErrResumeUnsupported = errors.New("adapter does not support resume")

// ErrUnavailable is returned when an adapter's backing tool is not usable
// (missing binary, failed auth, unsupported version).
var ErrUnavailable = errors.New("adapter backing tool unavailable")
