package main

import (
	"context"
	"errors"

	"github.com/levmv/golems/pkg/golem"
	"github.com/levmv/golems/pkg/llm"
)

// Exit codes. Anything that only tests for zero is unaffected. The classes are
// the three a caller has to act on differently: fix the invocation and rerun,
// wait and rerun unchanged, or look at what happened.
//
// 2 is already what flag.Parse exits with for a bad flag, and 130 is the
// shell's convention for a program ended by an interrupt, so both keep their
// usual meanings here.
const (
	exitOK          = 0
	exitFailure     = 1
	exitConfig      = 2
	exitProvider    = 3
	exitInterrupted = 130
)

// configError marks a failure that rerunning unchanged cannot fix: a malformed
// flag value, a missing credential, a workspace that is not the session's. It
// carries no message of its own, so what the user reads is unchanged and only
// the exit code differs.
type configError struct{ err error }

func (e configError) Error() string { return e.err.Error() }

func (e configError) Unwrap() error { return e.err }

func asConfigError(err error) error {
	if err == nil {
		return nil
	}
	return configError{err: err}
}

// exitCodeFor classifies by error type rather than by message text, so that
// rewording an error cannot silently change what a script does with it.
func exitCodeFor(err error) int {
	if err == nil {
		return exitOK
	}
	// Checked before the rest because an interrupt arrives during whatever the
	// run happened to be doing, and would otherwise be reported as that.
	if errors.Is(err, context.Canceled) {
		return exitInterrupted
	}
	var cfgErr configError
	if errors.As(err, &cfgErr) {
		return exitConfig
	}
	// A request the provider will reject however often it is sent is the user's
	// to fix, not the provider's to recover from.
	if errors.Is(err, llm.ErrInvalidRequest) {
		return exitConfig
	}
	// A tool that cannot be started is a mistake in the tool configuration --
	// a program that is not there, or not executable. Rerunning unchanged
	// fails the same way, which is what this code means.
	if errors.Is(err, golem.ErrToolFatal) {
		return exitConfig
	}
	var providerErr *llm.Error
	if errors.As(err, &providerErr) {
		return exitProvider
	}
	// A stalled stream, and a request that spent its whole budget without an
	// answer, are the provider not answering in the shape that matters most to an
	// unattended caller: the endpoint accepted the connection and then produced
	// nothing. Both are raised above the provider layer, so they carry no
	// *llm.Error and would otherwise read as unclassified failures -- the opposite
	// classification from a refused connection, for the same dead endpoint.
	if errors.Is(err, golem.ErrStreamIdle) || errors.Is(err, golem.ErrRequestBudget) {
		return exitProvider
	}
	return exitFailure
}
