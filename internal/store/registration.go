package store

// createRegistrationUserLocked mutates only the caller's transaction snapshot.
// It must never call another Store method or perform external I/O.
func (s *Store) createRegistrationUserLocked(u User, regCode string, bind BindCode, now int64, fn func(*User, RegCode, BindCode) error) (User, RegCode, error) {
	if s.usernameExistsLocked(u.Username) || s.emailTakenLocked(u.Email, 0) || s.embyIDTakenLocked(u.EmbyID, 0) {
		return User{}, RegCode{}, ErrConflict
	}
	if bind.Code != "" {
		if bind.ExpiresAt <= now {
			return User{}, RegCode{}, ErrExpired
		}
		if bind.Scene != "register" || !bind.Confirmed || bind.TelegramID <= 0 {
			return User{}, RegCode{}, ErrConflict
		}
		u.TelegramID, u.TelegramUsername = bind.TelegramID, bind.TelegramUsername
	}
	if s.telegramIDTakenLocked(u.TelegramID, 0) {
		return User{}, RegCode{}, ErrConflict
	}
	var consumed RegCode
	if regCode != "" {
		reg, err := s.consumableRegCodeLocked(regCode, 0, 0, now)
		if err != nil {
			return User{}, RegCode{}, err
		}
		if reg.Type != 1 || reg.IsDecoy || !regCodeMatchesUser(reg, u) {
			return User{}, RegCode{}, ErrNotFound
		}
		consumed = reg
	}
	u.UID = s.state.NextUserID
	s.state.NextUserID++
	if u.CreatedAt == 0 {
		u.CreatedAt = now
	}
	if u.RegisterTime == 0 {
		u.RegisterTime = now
	}
	if u.ExpiredAt == 0 {
		u.ExpiredAt = -1
	}
	u.Active = true
	if consumed.Code != "" {
		consumed = s.consumeRegCodeLocked(consumed, u.UID, u.TelegramID)
	}
	if fn != nil {
		if err := fn(&u, consumed, bind); err != nil {
			return User{}, RegCode{}, err
		}
	}
	if s.embyIDTakenLocked(u.EmbyID, u.UID) || s.telegramIDTakenLocked(u.TelegramID, u.UID) || s.emailTakenLocked(u.Email, u.UID) || s.usernameExistsLocked(u.Username) {
		return User{}, RegCode{}, ErrConflict
	}
	s.state.Users[u.UID] = u
	s.maintainUserIndexes(User{}, u, u.UID)
	return u, consumed, nil
}
