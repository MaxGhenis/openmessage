package app

import "strings"

const googleAuthExpiredStatusMessage = "Google Messages session cookie expired; refreshing and reconnecting..."

// IsGoogleAuthExpiredError reports whether a Google Messages API error means
// the linked-device web cookies/session are no longer accepted by Google.
func IsGoogleAuthExpiredError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "http 401") ||
		strings.Contains(msg, "session_cookie_invalid") ||
		strings.Contains(msg, "session cookie expired") ||
		strings.Contains(msg, "invalid authentication credentials")
}

// HandleGoogleAuthExpiredError marks Google disconnected for auth-expiry errors
// so the reconnect watchdog can refresh cookies and reconnect. It deliberately
// does NOT clear the needs-repair flag: whether an expired session is
// recoverable depends on a cookie-refresh script being configured, and that
// decision lives in the watchdog (see planGoogleReconnect). Clearing it here
// would defeat the watchdog's back-off and, with no refresh script (e.g. the
// macOS app), spin a reconnect storm against Google's auth endpoint.
func (a *App) HandleGoogleAuthExpiredError(err error) bool {
	if !IsGoogleAuthExpiredError(err) {
		return false
	}
	a.Connected.Store(false)
	a.googleAuthExpired.Store(true)
	a.setGoogleLastError(googleAuthExpiredStatusMessage)
	a.emitStatusChange(false)
	a.Logger.Warn().Err(err).Msg("Google auth expired; marking disconnected")
	return true
}
