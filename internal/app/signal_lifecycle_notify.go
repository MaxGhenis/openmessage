package app

// SignalLifecycleNotifier lets legacy App send paths report signal-cli
// transport failures without owning receive-loop reconnect or parking.
type SignalLifecycleNotifier interface {
	ReportError(error) bool
}

// SetSignalLifecycleNotifier installs the sole lifecycle owner notified by
// Signal send/media/reaction paths. It is safe to replace or clear while an
// operation is in flight.
func (a *App) SetSignalLifecycleNotifier(notifier SignalLifecycleNotifier) {
	a.signalLifecycleMu.Lock()
	a.signalLifecycleNotifier = notifier
	a.signalLifecycleMu.Unlock()
}

func (a *App) signalLifecycle() SignalLifecycleNotifier {
	a.signalLifecycleMu.RLock()
	notifier := a.signalLifecycleNotifier
	a.signalLifecycleMu.RUnlock()
	return notifier
}

func (a *App) reportSignalLifecycleError(err error) {
	if notifier := a.signalLifecycle(); notifier != nil {
		notifier.ReportError(err)
	}
}
