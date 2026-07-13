package app

// WhatsAppLifecycleNotifier lets the legacy WhatsApp bridge report transport
// failures without owning reconnect or repair. The WhatsApp bridge adapter
// implements this interface and leaves generation changes to the supervisor.
type WhatsAppLifecycleNotifier interface {
	// ReportError ends the current generation with an error classified by the
	// supervisor-owned lifecycle adapter.
	ReportError(error) bool
}

// SetWhatsAppLifecycleNotifier installs the lifecycle owner notified by the
// legacy WhatsApp bridge. It is safe to replace or clear the notifier while
// bridge operations are in flight.
func (a *App) SetWhatsAppLifecycleNotifier(notifier WhatsAppLifecycleNotifier) {
	a.whatsAppLifecycleMu.Lock()
	a.whatsAppLifecycleNotifier = notifier
	a.whatsAppLifecycleMu.Unlock()
}

func (a *App) whatsAppLifecycle() WhatsAppLifecycleNotifier {
	a.whatsAppLifecycleMu.RLock()
	notifier := a.whatsAppLifecycleNotifier
	a.whatsAppLifecycleMu.RUnlock()
	return notifier
}

func (a *App) reportWhatsAppLifecycleError(err error) {
	if notifier := a.whatsAppLifecycle(); notifier != nil {
		notifier.ReportError(err)
	}
}
