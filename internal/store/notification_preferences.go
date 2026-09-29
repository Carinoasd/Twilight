package store

// Missing historical preferences retain the existing Telegram expiry reminders.
// Replace the pointer when editing; never modify its shared pointed-to value.
func (u User) ExpiryTelegramNotificationsEnabled() bool {
	return u.NotifyOnExpiryTelegram == nil || *u.NotifyOnExpiryTelegram
}
