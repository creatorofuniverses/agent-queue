package cli

import (
	"github.com/creatorofuniverses/agent-queue/internal/daemon"
	"github.com/creatorofuniverses/agent-queue/internal/model"
)

// Process exit codes.
const (
	exitOK               = 0
	exitFailed           = 1
	exitOOM              = 2
	exitTimeout          = 3
	exitCancelled        = 4
	exitInfra            = 5
	exitReservationEnded = 6
	exitUsage            = 64
	exitUnavailable      = 69
	exitInterrupted      = 130
)

// ExitCodeFor maps a finished item to the exit code of `aq wait`.
func ExitCodeFor(v daemon.ItemView) int {
	if v.Kind == model.KindReservation {
		switch v.State {
		case model.StateGranted, model.StateExpiring:
			return exitOK
		case model.StateCancelled:
			return exitCancelled
		case model.StateRejected:
			return exitInfra
		}
		return exitReservationEnded // no longer held
	}
	switch v.Reason {
	case model.ReasonCommandFailed:
		return exitFailed
	case model.ReasonOOM, model.ReasonOOMGroup, model.ReasonOOMHostGuard:
		return exitOOM
	case model.ReasonTimeout:
		return exitTimeout
	case model.ReasonCancelled:
		return exitCancelled
	case model.ReasonLaunchFailed, model.ReasonLost, model.ReasonStorageFull:
		return exitInfra
	}
	switch v.State {
	case model.StateSucceeded:
		return exitOK
	case model.StateCancelled:
		return exitCancelled
	case model.StateRejected:
		return exitInfra
	}
	return exitFailed
}
