package cli

import (
	"os"
	"os/signal"
	"sync/atomic"
)

// The backend owns delivery to its child. This watcher extends MoE's SIGINT
// lifetime across preparation, commit and teardown after the backend's Wait.
// The first interrupt latches cancellation; the second may force termination,
// deliberately leaving an unreleased claim subject to crash recovery.
type stageInterrupt struct {
	interrupted atomic.Bool
	stop        func()
}

func watchStageInterrupt() *stageInterrupt {
	signals := make(chan os.Signal, 2)
	signal.Notify(signals, os.Interrupt)
	w := &stageInterrupt{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for range signals {
			if w.interrupted.Swap(true) {
				os.Exit(exitInterrupted)
			}
		}
	}()
	w.stop = func() { signal.Stop(signals); close(signals); <-done }
	return w
}
