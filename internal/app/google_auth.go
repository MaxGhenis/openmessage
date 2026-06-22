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

// HandleGoogleAuthExpiredError marks Google disconnected for auth-expiry
// errors so external watchdogs can refresh cookies and reconnect.
func (a *App) HandleGoogleAuthExpiredError(err error) bool {
	if !IsGoogleAuthExpiredError(err) {
		return false
	}
	a.Connected.Store(false)
	a.ClearGoogleRepairFlag()
	a.googleAuthExpired.Store(true)
	a.setGoogleLastError(googleAuthExpiredStatusMessage)
	a.emitStatusChange(false)
	a.Logger.Warn().Err(err).Msg("Google auth expired; marking disconnected")
	return true
}
