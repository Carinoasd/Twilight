// 会话失效与跨分页身份同步的纯逻辑（不依赖 React / Next，便于 node --test）。

export interface SessionExpiredInfo {
  endpoint: string;
  status: number;
  errorCode?: string;
}

type SessionExpiredListener = (info: SessionExpiredInfo) => void;

const listeners = new Set<SessionExpiredListener>();

// 登录 / 登出 / 注册 / 找回密码等接口的 401 表示凭据错误，不是会话过期。
const AUTH_FLOW_PREFIXES = ["/auth/", "/registration", "/users/register"];

/**
 * 判断一次失败响应是否意味着「当前会话已失效」。
 * 原来只有 /users/me 的 401 会登出，其它接口的 401 只弹 toast，用户停留在一个
 * 看似已登录、实际所有操作都失败的页面。现在任何业务接口返回会话失效的 401
 * （error_code=UNAUTHORIZED 或缺失）都统一登出并回到登录页。
 */
export function shouldTreatAsSessionExpired(status: number, errorCode: string | undefined, endpoint: string): boolean {
  if (status !== 401) return false;
  if (errorCode && errorCode !== "UNAUTHORIZED") return false;
  const path = endpoint.split("?")[0] || "";
  return !AUTH_FLOW_PREFIXES.some((prefix) => path.startsWith(prefix));
}

export function onSessionExpired(listener: SessionExpiredListener): () => void {
  listeners.add(listener);
  return () => {
    listeners.delete(listener);
  };
}

export function emitSessionExpired(info: SessionExpiredInfo): void {
  for (const listener of Array.from(listeners)) {
    try {
      listener(info);
    } catch {
      // 监听者异常不影响请求本身的错误抛出
    }
  }
}

// ---- 跨分页身份同步 ----

export const AUTH_CHANNEL_NAME = "twilight-auth";

export type AuthChannelMessage =
  | { type: "identity"; uid: number }
  | { type: "logout" };

export function isAuthChannelMessage(value: unknown): value is AuthChannelMessage {
  if (typeof value !== "object" || value === null) return false;
  const record = value as Record<string, unknown>;
  if (record.type === "logout") return true;
  return record.type === "identity" && typeof record.uid === "number" && Number.isFinite(record.uid);
}

/**
 * 收到其它分页的广播后本分页该做什么：
 *   - 其它分页登出 → 本分页也登出；
 *   - 其它分页登录成另一个 uid → 本分页重新加载（cookie 已是新账号，旧页面状态
 *     继续操作会作用在新账号上）；
 *   - 同一 uid 或本分页未登录 → 不处理（未登录分页会在下次进入受保护页时自行确认）。
 */
export function decideCrossTabAction(currentUid: number | null | undefined, message: AuthChannelMessage): "none" | "logout" | "reload" {
  if (message.type === "logout") return currentUid ? "logout" : "none";
  if (!currentUid) return "none";
  return message.uid === currentUid ? "none" : "reload";
}
