// Ranking images use existing authenticated resources. Do not load arbitrary
// legacy profile URLs or send Emby credentials to the browser.
export function playRankImageUrl(value: string | undefined, kind: "poster" | "avatar"): string | undefined {
  if (!value) return undefined;
  const pattern = kind === "poster"
    ? /^\/api\/v2\/emby\/items\/[a-zA-Z0-9_-]{1,128}\/image$/
    : /^\/api\/v2\/emby\/play-rank\/avatars\/[a-f0-9]{16}\.(jpg|png|gif|webp|bmp)$/;
  return pattern.test(value) ? value : undefined;
}
