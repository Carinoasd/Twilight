// 求片海报的主机白名单。求片记录里的 poster 历史上可由客户端任意填写，管理员打开
// 求片页时浏览器会去请求这些地址（泄漏 IP / UA / 上线时间）。后端现在只保存白名单
// 主机的地址；前端再做一次同样的过滤，挡住旧数据里残留的外站地址。
const TRUSTED_POSTER_HOSTS = ["image.tmdb.org"];
const TRUSTED_POSTER_HOST_SUFFIXES = [".bgm.tv", ".bangumi.tv"];

export function isTrustedMediaPosterHost(host: string): boolean {
  const value = host.toLowerCase();
  return (
    TRUSTED_POSTER_HOSTS.includes(value) ||
    TRUSTED_POSTER_HOST_SUFFIXES.some((suffix) => value.endsWith(suffix))
  );
}

// 返回可以直接放进 <img src> 的海报地址；不可信时返回 undefined。
// 允许：https + 白名单主机，或本站相对路径（以单个 / 开头）。
export function sanitizeMediaPosterUrl(url?: string | null): string | undefined {
  if (!url) return undefined;
  const value = url.trim();
  if (!value || /[\u0000-\u001F\u007F\s]/.test(value)) return undefined;
  if (value.startsWith("/") && !value.startsWith("//") && !value.startsWith("/\\")) return value;
  try {
    const parsed = new URL(value);
    if (parsed.protocol !== "https:" || parsed.username || parsed.password) return undefined;
    if (parsed.port && parsed.port !== "443") return undefined;
    return isTrustedMediaPosterHost(parsed.hostname) ? parsed.toString() : undefined;
  } catch {
    return undefined;
  }
}
