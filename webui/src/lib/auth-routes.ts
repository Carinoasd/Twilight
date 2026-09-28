// 必须覆盖 (main) 下的全部页面，否则登录后的 next 回跳会被当成不安全目标丢弃。
export const PROTECTED_ROUTE_PREFIXES = [
  "/dashboard",
  "/admin",
  "/announcements",
  "/bangumi",
  "/invite",
  "/media",
  "/playrank",
  "/score",
  "/settings",
  "/tickets",
];

export const AUTH_ROUTE_PREFIXES = ["/login", "/register", "/forgot-password"];

export function pathMatches(pathname: string, prefixes: readonly string[]): boolean {
  return prefixes.some((prefix) => pathname === prefix || pathname.startsWith(`${prefix}/`));
}

export function safeProtectedRedirectTarget(
  rawNext: string | null | undefined,
  fallback = "/dashboard",
): string {
  const next = rawNext?.trim();
  if (!next || !next.startsWith("/") || next.startsWith("//")) return fallback;

  try {
    const url = new URL(next, "https://twilight.local");
    if (url.origin !== "https://twilight.local") return fallback;
    if (!pathMatches(url.pathname, PROTECTED_ROUTE_PREFIXES)) return fallback;
    return `${url.pathname}${url.search}${url.hash}`;
  } catch {
    return fallback;
  }
}

/**
 * 受保护页发现未登录时的登录地址：带上当前路径作为 next，登录后回到原页面。
 * 原实现直接 push("/login")，登录页的 next 回跳逻辑因此从未生效。
 */
export function buildLoginRedirect(pathname: string, search = ""): string {
  const query = search && !search.startsWith("?") ? `?${search}` : search;
  const target = safeProtectedRedirectTarget(`${pathname}${query}`, "");
  if (!target || target === "/dashboard") return "/login";
  return `/login?next=${encodeURIComponent(target)}`;
}
