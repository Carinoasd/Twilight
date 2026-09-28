// CSP 生成逻辑（纯函数，便于测试）。由 src/proxy.ts 按请求调用。
//
// script-src 维持 'self' 'unsafe-inline'：Next 16 的内联 bootstrap 脚本不带
// nonce，而 CSP3 规定 source-list 一旦含 nonce-source 就忽略 'unsafe-inline'，
// 加 nonce 或 'strict-dynamic' 都会让内联脚本被拒、页面白屏（详见 proxy.ts 顶部
// 注释）。在 Next 能稳定为全部内联脚本打 nonce / hash 之前无法移除。
//
// img-src 原为 `'self' data: blob: https:`，任何 https 图片都能加载，求片海报等
// 用户可控地址可以借此追踪管理员。现改为白名单：本站、API 源、TMDB 与 Bangumi
// 图床；自建 TMDB 图片代理或其它图床用 NEXT_PUBLIC_CSP_IMG 追加（空格分隔的 origin）。

export interface CspOptions {
  isDev: boolean;
  requestOrigin: string;
  apiUrl?: string;
  extraConnect?: string;
  extraImg?: string;
}

export const DEFAULT_IMG_SOURCES = [
  "https://image.tmdb.org",
  "https://bgm.tv",
  "https://*.bgm.tv",
  "https://*.bangumi.tv",
];

// safeOrigin 把完整 URL 规约为 scheme + host + port 的纯 origin；非 http(s) / 无法解析返回空串。
export function safeOrigin(raw: string | undefined): string {
  const trimmed = raw?.trim();
  if (!trimmed) return "";
  try {
    const u = new URL(trimmed);
    if (u.protocol !== "https:" && u.protocol !== "http:") return "";
    return u.origin;
  } catch {
    return "";
  }
}

function webSocketOriginFromHTTPOrigin(origin: string): string {
  try {
    const u = new URL(origin);
    u.protocol = u.protocol === "https:" ? "wss:" : "ws:";
    return u.origin;
  } catch {
    return "";
  }
}

// 额外白名单每项都必须是 http(s) origin，"*"、"https:" 之类全通配直接丢弃。
function addExtraOrigins(target: Set<string>, raw: string | undefined) {
  const value = raw?.trim();
  if (!value) return;
  for (const piece of value.split(/\s+/)) {
    const origin = safeOrigin(piece);
    if (origin) target.add(origin);
  }
}

export function buildContentSecurityPolicy(options: CspOptions): string {
  // dev 下 Next 用 eval 做 HMR / RSC payload 解析；生产构建后丢掉 unsafe-eval。
  const scriptExtras = options.isDev ? " 'unsafe-eval'" : "";
  const apiOrigin = safeOrigin(options.apiUrl);

  const connectParts = new Set<string>(["'self'"]);
  if (apiOrigin) connectParts.add(apiOrigin);
  const selfWsOrigin = webSocketOriginFromHTTPOrigin(options.requestOrigin);
  if (selfWsOrigin) connectParts.add(selfWsOrigin);
  const apiWsOrigin = apiOrigin ? webSocketOriginFromHTTPOrigin(apiOrigin) : "";
  if (apiWsOrigin) connectParts.add(apiWsOrigin);
  connectParts.add("https://cloudflareinsights.com");
  addExtraOrigins(connectParts, options.extraConnect);

  const imgParts = new Set<string>(["'self'", "data:", "blob:"]);
  if (apiOrigin) imgParts.add(apiOrigin);
  for (const source of DEFAULT_IMG_SOURCES) imgParts.add(source);
  addExtraOrigins(imgParts, options.extraImg);

  return [
    "default-src 'self'",
    `script-src 'self' 'unsafe-inline' https://static.cloudflareinsights.com${scriptExtras}`,
    "style-src 'self' 'unsafe-inline'",
    `img-src ${Array.from(imgParts).join(" ")}`,
    "font-src 'self' data:",
    `connect-src ${Array.from(connectParts).join(" ")}`,
    "media-src 'self' https:",
    "frame-ancestors 'none'",
    "object-src 'none'",
    "base-uri 'self'",
    "form-action 'self'",
    "worker-src 'self' blob:",
    "manifest-src 'self'",
    "upgrade-insecure-requests",
  ].join("; ");
}
