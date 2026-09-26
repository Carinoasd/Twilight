package api

import (
	"context"
	"database/sql"
	"net/http"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

func testTelegramCookies(code string) []*http.Cookie {
	return []*http.Cookie{{Name: telegramBrowserCookie, Value: store.TelegramChallengeHash(code)}}
}

// Legacy tests deliberately construct expired, confirmed and orphan records.
// Keep this fixture-only SQL outside the strict production issuance API.
func (a *App) upsertBindCode(bind store.BindCode) error {
	a.cfg().TelegramMode = true
	if a.cfg().TelegramBotToken == "" {
		a.cfg().TelegramBotToken = "123:fixture"
	}
	db, err := sql.Open("pgx", testDSN)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if bind.CreatedAt == 0 {
		bind.CreatedAt = time.Now().Unix()
	}
	state := "pending"
	if bind.Confirmed {
		state = "verified"
		if bind.UID > 0 {
			state = "consumed"
		}
	}
	_, err = db.ExecContext(ctx, `INSERT INTO twilight_telegram_challenges (id,token_hash,owner_hash,uid,scene,state,telegram_id,telegram_username,created_at,expires_at)
 VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10) ON CONFLICT (id) DO UPDATE SET state=EXCLUDED.state,telegram_id=EXCLUDED.telegram_id,telegram_username=EXCLUDED.telegram_username,expires_at=EXCLUDED.expires_at,revision=twilight_telegram_challenges.revision+1`,
		store.TelegramChallengeHash(bind.Code)[:32], store.TelegramChallengeHash(bind.Code), store.TelegramBrowserHash(store.TelegramChallengeHash(bind.Code)), bind.UID, bind.Scene, state, bind.TelegramID, bind.TelegramUsername, bind.CreatedAt, bind.ExpiresAt)
	if err == nil {
		a.bindStatus.notify(bind.Code)
	}
	return err
}

func (a *App) telegramBindCodeState(code string, uid int64, scene string, now int64, cleanup bool) telegramBindCodeState {
	return a.telegramBindCodeStateContext(context.Background(), code, uid, scene, now)
}

func (a *App) bindCode(code string) (store.BindCode, bool) {
	c, err := a.store().TelegramChallenge(context.Background(), code)
	return c.BindCode, err == nil && c.State != "cancelled" && !(c.Scene == "register" && c.State == "consumed")
}

func (a *App) deleteBindCode(code string) error {
	err := a.store().DeleteTelegramChallenge(context.Background(), code)
	if err == nil && a.bindStatus != nil {
		a.bindStatus.notify(code)
	}
	return err
}

func (a *App) confirmBindCodeAtomic(code string, telegramID int64, username string, now int64) (store.BindCode, store.User, bool, error) {
	c, u, changed, err := a.store().ConfirmTelegramChallenge(context.Background(), code, telegramID, username)
	if err == nil {
		if a.bindStatus != nil {
			a.bindStatus.notify(c.ID)
			a.bindStatus.notify(code)
		}
		if changed {
			a.auditTelegramAction(telegramID, "bind_telegram_via_telegram", "user", u.UID, map[string]any{"scene": c.Scene})
		}
	}
	return c.BindCode, u, changed, err
}
