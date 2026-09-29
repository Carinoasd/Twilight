package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/prejudice-studio/twilight/internal/store"
)

func TestTelegramRebindPermissionEndsAfterBinding(t *testing.T) {
	for _, assisted := range []bool{false, true} {
		for _, forceBind := range []bool{false, true} {
			for _, targetID := range []int64{111, 222} {
				t.Run(fmt.Sprintf("assisted=%t/force=%t/target=%d", assisted, forceBind, targetID), func(t *testing.T) {
					app := newTestApp(t)
					adminCookies := registerAndLogin(t, app, "admin", "Admin123456")
					newFakeTelegramServer(t, app)
					u, cookies := approvedRebindUser(t, app, "rebinder", "", false)
					approved, _ := app.store().UserLatestRebindRequest(u.UID)
					app.cfg().ForceBindTelegram = forceBind
					headers := map[string]string{"X-Twilight-Client": "webui"}
					path, actor := "/api/v2/telegram/unbind", cookies
					if assisted {
						path, actor = fmt.Sprintf("/api/v1/admin/users/%d/unbind-telegram", u.UID), adminCookies
					}
					if rr := doJSONWithHeaders(app, http.MethodPost, path, "", actor, headers); rr.Code != http.StatusOK {
						t.Fatalf("unbind: %d %s", rr.Code, rr.Body.String())
					}
					link := issueUserTelegramLink(t, app, cookies)
					bot := newBotApp(t, app)
					if result := bot.confirmTelegramLink(context.Background(), link.Token, targetID, "rebound"); !result.Success {
						t.Fatalf("confirm: %+v", result)
					}
					status := doJSON(app, http.MethodGet, "/api/v2/telegram/status", "", cookies)
					var response struct {
						Data telegramStatusResult `json:"data"`
					}
					if status.Code != http.StatusOK || json.Unmarshal(status.Body.Bytes(), &response) != nil {
						t.Fatalf("status: %d %s", status.Code, status.Body.String())
					}
					if response.Data.CanUnbind || response.Data.RebindApproved || response.Data.RebindingInProgress {
						t.Errorf("permission survives completed rebind: %+v", response.Data)
					}
					used, _ := app.store().UserLatestRebindRequest(u.UID)
					if used.Status != "used" || used.ReviewerUID != approved.ReviewerUID || used.ReviewedAt != approved.ReviewedAt || used.AdminNote != approved.AdminNote {
						t.Errorf("approval not consumed with audit preserved: %+v", used)
					}
					if rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/telegram/unbind", "", cookies, headers); rr.Code != http.StatusForbidden {
						t.Errorf("second unbind: %d %s", rr.Code, rr.Body.String())
					}
					// A new request and review still authorize exactly one new cycle.
					if rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/telegram/rebind-request", `{}`, cookies, headers); rr.Code != http.StatusOK {
						t.Fatalf("new request: %d %s", rr.Code, rr.Body.String())
					}
					next, _ := app.store().UserLatestRebindRequest(u.UID)
					if next.ID == approved.ID || next.Status != "pending" {
						t.Fatalf("new cycle reused old request: %+v", next)
					}
					if rr := doJSONWithHeaders(app, http.MethodPost, fmt.Sprintf("/api/v2/admin/telegram/rebind-requests/%d/approve", next.ID), `{}`, adminCookies, headers); rr.Code != http.StatusOK {
						t.Fatalf("fresh approval: %d %s", rr.Code, rr.Body.String())
					}
					if rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/telegram/unbind", "", cookies, headers); rr.Code != http.StatusOK {
						t.Fatalf("new authorized cycle: %d %s", rr.Code, rr.Body.String())
					}
				})
			}
		}
	}
}

func TestTelegramRebindConsumedApprovalCannotBeReviewedAgain(t *testing.T) {
	for _, batch := range []bool{false, true} {
		t.Run(fmt.Sprintf("batch=%t", batch), func(t *testing.T) {
			app := newTestApp(t)
			adminCookies := registerAndLogin(t, app, "admin", "Admin123456")
			u, cookies := approvedRebindUser(t, app, "rebinder", "", false)
			request, _ := app.store().UserLatestRebindRequest(u.UID)
			headers := map[string]string{"X-Twilight-Client": "webui"}
			if rr := doJSONWithHeaders(app, http.MethodPost, "/api/v2/telegram/unbind", "", cookies, headers); rr.Code != http.StatusOK {
				t.Fatal(rr.Body.String())
			}
			if _, err := app.store().BindUnboundUserTelegram(u.UID, 111, "same"); err != nil {
				t.Fatal(err)
			}
			path := fmt.Sprintf("/api/v2/admin/telegram/rebind-requests/%d/approve", request.ID)
			body := `{"admin_note":"replayed"}`
			if batch {
				path = "/api/v2/admin/telegram/rebind-requests/batch"
				body = fmt.Sprintf(`{"ids":[%d],"action":"approve","admin_note":"replayed"}`, request.ID)
			}
			rr := doJSONWithHeaders(app, http.MethodPost, path, body, adminCookies, headers)
			if batch {
				var result struct {
					Data struct {
						Success int `json:"success"`
						Failed  int `json:"failed"`
					} `json:"data"`
				}
				if rr.Code != http.StatusOK || json.Unmarshal(rr.Body.Bytes(), &result) != nil || result.Data.Success != 0 || result.Data.Failed != 1 {
					t.Errorf("batch review replay: %d %s", rr.Code, rr.Body.String())
				}
			}
			if !batch && rr.Code != http.StatusConflict {
				t.Errorf("review replay: %d %s", rr.Code, rr.Body.String())
			}
			got, _ := app.store().UserLatestRebindRequest(u.UID)
			if got.Status != "used" || got.AdminNote != request.AdminNote {
				t.Errorf("used request resurrected: %+v", got)
			}
			if _, err := app.store().UnbindUserTelegram(u.UID, 111); err != store.ErrTelegramRebindApprovalRequired {
				t.Errorf("replayed review grants another unbind: %v", err)
			}
		})
	}
}
