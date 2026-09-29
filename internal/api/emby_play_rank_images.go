package api

import (
	"context"
	"net/http"
	"strings"
	"time"

	"github.com/prejudice-studio/twilight/internal/store"
)

// Artwork is optional: an unavailable Emby must not hold up the local ranking.
func (a *App) playRankMetadata(ctx context.Context, media []store.PlaybackMediaRank, groupBy string) map[string]embyItemMetadata {
	if groupBy == playRankGroupMovie || !a.embyConfigured() {
		return nil
	}
	ids := make([]string, 0, min(len(media), playRankMaxLimit))
	for _, item := range media {
		id := firstNonEmpty(item.RepresentativeItemID, item.ItemID)
		if validEmbyItemID(id) {
			ids = append(ids, id)
		}
		if len(ids) == playRankMaxLimit {
			break
		}
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	return a.embyItemMetadata(ctx, ids)
}

func playRankPosterURL(item store.PlaybackMediaRank, groupBy string, metadata map[string]embyItemMetadata) string {
	id := item.ItemID
	if groupBy == playRankGroupSeries {
		meta := metadata[firstNonEmpty(item.RepresentativeItemID, item.ItemID)]
		// Never substitute an episode still when the parent series is unknown.
		id = meta.SeriesID
		if strings.EqualFold(meta.Type, "Series") {
			id = meta.ID
		}
	}
	if !validEmbyItemID(id) {
		return ""
	}
	return "/api/v2/emby/items/" + id + "/image"
}

// Only uploaded, opaque asset names are shared. Legacy external URLs can contain
// account identifiers or tracking endpoints; do not expose them in the ranking.
func playRankAvatarURL(avatar string) string {
	const prefix = "/api/v1/users/assets/avatar/"
	if !strings.HasPrefix(avatar, prefix) || !uploadFilenamePattern.MatchString(strings.TrimPrefix(avatar, prefix)) {
		return ""
	}
	return "/api/v2/emby/play-rank/avatars/" + strings.TrimPrefix(avatar, prefix)
}

// This read has the ranking audience; personal asset reads keep their original
// owner/admin restriction. Only a currently assigned avatar of a viewer is shared.
func (a *App) handleV2PlayRankAvatar(w http.ResponseWriter, r *http.Request, params Params) {
	w.Header().Set("Cache-Control", "private, no-store")
	if current(r).User.Role != store.RoleAdmin && (!a.cfg().PlayRankEnabled || !a.cfg().PlayRankUserVisible) {
		failWithCode(w, http.StatusForbidden, ErrForbidden, "排行榜未对普通用户开放")
		return
	}
	filename := params["filename"]
	if !uploadFilenamePattern.MatchString(filename) || !a.store().PlaybackAvatarVisible(r.Context(), "/api/v1/users/assets/avatar/"+filename) {
		failWithCode(w, http.StatusNotFound, ErrAssetNotFound, "resource not found")
		return
	}
	filePath, valid := resolveUploadAssetPath(a.cfg().UploadDir, "avatar", filename)
	if !valid {
		failWithCode(w, http.StatusNotFound, ErrAssetNotFound, "resource not found")
		return
	}
	http.ServeFile(w, r, filePath)
}
