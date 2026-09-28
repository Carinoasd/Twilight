import { NextRequest, NextResponse } from "next/server";
import { buildContentSecurityPolicy } from "@/lib/csp";

/**
 * Proxy：只注入 CSP，不再根据 session cookie 做服务端跳转。
 *
 * ## CSP（脚本部分）
 *
 * 当前生产环境用 `script-src 'self' 'unsafe-inline'`。它是"在 Next 16 上能
 * 真正跑通的最严格策略"——并不是没尝试更紧的：
 *
 *   1. 最初为了挡反射型 XSS，曾改成 `script-src 'self' 'nonce-XXX' 'strict-dynamic'`。
 *      但 `'strict-dynamic'` 让浏览器忽略 `'self'`，而 Next 16 自动 nonce
 *      注入对 `_next/static/chunks/...` 偶尔漏标，生产出现整段 chunk 被
 *      拒绝、整页白屏（vercel/next.js 已知 issue）。
 *   2. 退到 `script-src 'self' 'nonce-XXX'`。chunk 走 'self' 通过了，
 *      但 Next 16 同样会塞内联 bootstrap `<script>` 不带 nonce——CSP3
 *      规范规定一旦 source-list 里有 nonce-source，`'unsafe-inline'`
 *      就会被忽略，于是这些内联脚本被全部拒绝、依赖 hydration 的页面
 *      整片报"Executing inline script violates ..."然后死透。
 *   3. 现在退到 `'self' 'unsafe-inline'`：与 Next 16 的内联 bootstrap +
 *      `_next/static/chunks` 共存的最简形式。等 Next 把 auto-nonce 修稳
 *      （或升级到内联脚本全 hash 化的版本）再回头收紧。
 *
 * 同源策略 + `frame-ancestors 'none'` + `object-src 'none'` 仍然挡掉了
 * `<iframe src=>` 嵌入与 Flash/PDF object 注入；XSS 表面 = "攻击者能写
 * 进同源 DOM"，与项目其他地方（HttpOnly cookie、后端输入校验）的纵深防御边界一致。
 *
 * 登录态只由客户端 layout 调 `/users/me` 让后端权威判定。过去在 proxy / root
 * / auth layout 多处用 Web 域 cookie 猜测登录态，遇到跨域 API、Cookie Domain、
 * SameSite 或浏览器持久化差异时容易把已登录用户反复送回 `/login`。
 */

export function proxy(request: NextRequest) {
  const csp = buildContentSecurityPolicy({
    isDev: process.env.NODE_ENV !== "production",
    requestOrigin: request.nextUrl.origin,
    apiUrl: process.env.NEXT_PUBLIC_API_URL,
    extraConnect: process.env.NEXT_PUBLIC_CSP_CONNECT,
    // 构建期配置的外部图标地址自动放行；后端配置的外部 server_icon / 登录背景需写进 NEXT_PUBLIC_CSP_IMG。
    extraImg: [
      process.env.NEXT_PUBLIC_CSP_IMG,
      process.env.NEXT_PUBLIC_AUTH_ICON_URL,
      process.env.NEXT_PUBLIC_SITE_ICON,
      process.env.NEXT_PUBLIC_LANDING_ICON,
    ].filter(Boolean).join(" "),
  });
  const response = NextResponse.next();
  response.headers.set("Content-Security-Policy", csp);
  return response;
}

export const config = {
  matcher: [
    // 跳过 _next 静态资源、图片优化、favicon 各格式、其它静态资产；
    // 这些请求永远不会触发脚本执行，跳过 proxy 节省每请求 CPU。
    // favicon.png 单独列出：iOS Safari / Android Chrome 都会无视 SVG 偏好直
    // 接发 GET /favicon.png 探活；如果它没在 matcher 排除里，每次浏览器开标
    // 签都要走一次 proxy（CSP 头注入 + cookie 解析），白白浪费 edge 算力。
    "/((?!_next/static|_next/image|favicon\\.ico|favicon\\.svg|favicon\\.png|images/|api/).*)",
  ],
};
