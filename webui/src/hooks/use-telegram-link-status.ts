"use client";

import { useEffect, useRef } from "react";
import { api } from "@/lib/api";
import type { TelegramLinkStatus } from "@/lib/api-types-v2";

export type TelegramLinkScene = "user" | "register";

export interface UseTelegramLinkStatusOptions {
  /** 绑定链接资源 ID（link_id）；为空时不订阅。 */
  linkId: string | null | undefined;
  /** 注册场景的浏览器 secret；已登录场景不需要。 */
  secret?: string;
  /** user = 个人设置 / 换绑，register = 注册。决定走哪组状态端点。默认 user。 */
  scene?: TelegramLinkScene;
  /** 链接初始有效期(秒)，用于设定整体看护上限(deadline)。 */
  expiresIn?: number;
  /** false 时暂停订阅（如已绑定）。默认 true。 */
  enabled?: boolean;
  /** 轮询间隔(ms)。默认 3000。 */
  pollIntervalMs?: number;
  /** 进入"已绑定 / 已确认"终态。 */
  onBound: (data: TelegramLinkStatus) => void;
  /** 进入"无效 / 过期 / 被占用"等失败终态。 */
  onTerminalError: (data: TelegramLinkStatus) => void;
  /** 整体看护超时：链接 TTL 到期仍未达终态时触发。 */
  onTimeout?: () => void;
}

function isBoundStatus(data: TelegramLinkStatus): boolean {
  if (data.invalid) return false;
  return Boolean(data.telegram_bound) || data.status === "confirmed" || Boolean(data.confirmed);
}

/**
 * useTelegramLinkStatus 统一 Telegram 绑定链接状态轮询（个人设置绑定 / 换绑 / 注册共用）。
 *
 *   1. 挂载即自动轮询到终态，不要求用户刷新页面；
 *   2. 整体 deadline = 链接 TTL + 宽限，到点强制停轮询并回调 onTimeout；
 *   3. 每次轮询用独立 AbortController，卸载 / linkId 变更 / 达终态立即 abort 在途请求；
 *   4. 页面不可见时暂停，回到前台按剩余间隔补一次；
 *   5. 临时的加群 / 上游失败（pending + retryable）继续轮询，只有服务端明确终态才停止。
 *
 * 服务端没有长轮询与 WebSocket：每次请求都是一次普通的数据库读取，多进程部署下
 * 独立 Bot 的确认结果同样可见。
 */
export function useTelegramLinkStatus(options: UseTelegramLinkStatusOptions): void {
  const optionsRef = useRef(options);
  optionsRef.current = options;

  const {
    linkId,
    secret = "",
    scene = "user",
    expiresIn,
    enabled = true,
    pollIntervalMs = 3000,
  } = options;

  useEffect(() => {
    const id = (linkId || "").trim();
    if (!id || !enabled) return;

    let stopped = false;
    let running = false;
    let pollTimer: ReturnType<typeof setTimeout> | null = null;
    let deadlineTimer: ReturnType<typeof setTimeout> | null = null;
    let controller: AbortController | null = null;
    let lastRunAt = 0;

    const isVisible = () => document.visibilityState === "visible";

    const fetchStatus = (signal: AbortSignal) =>
      scene === "register"
        ? api.getRegisterTelegramLinkStatus(id, secret, signal)
        : api.getTelegramLinkStatus(id, signal);

    const stop = () => {
      if (stopped) return;
      stopped = true;
      if (pollTimer) clearTimeout(pollTimer);
      if (deadlineTimer) clearTimeout(deadlineTimer);
      pollTimer = null;
      deadlineTimer = null;
      controller?.abort();
      controller = null;
    };

    const schedule = (delay = pollIntervalMs) => {
      if (stopped) return;
      if (pollTimer) clearTimeout(pollTimer);
      pollTimer = null;
      if (!isVisible()) return;
      pollTimer = setTimeout(() => {
        void poll();
      }, Math.max(0, delay));
    };

    const handle = (data: TelegramLinkStatus) => {
      if (stopped) return;
      if (isBoundStatus(data)) {
        stop();
        optionsRef.current.onBound(data);
        return;
      }
      if (data.terminal && data.status !== "pending") {
        stop();
        optionsRef.current.onTerminalError(data);
      }
    };

    const poll = async () => {
      if (stopped || running) return;
      if (!isVisible()) {
        if (pollTimer) clearTimeout(pollTimer);
        pollTimer = null;
        return;
      }
      running = true;
      lastRunAt = Date.now();
      controller = new AbortController();
      try {
        const res = await fetchStatus(controller.signal);
        // 终态失败以 HTTP 200 + success=false 返回，data 仍是唯一可信的生命周期信号。
        if (!stopped && res.data) {
          handle(res.data as TelegramLinkStatus);
        }
      } catch {
        // 网络抖动 / 单次超时 / 503：忽略，保持轮询直到 deadline。
      }
      controller = null;
      running = false;
      schedule();
    };

    const handleVisibility = () => {
      if (stopped) return;
      if (!isVisible()) {
        if (pollTimer) clearTimeout(pollTimer);
        pollTimer = null;
        controller?.abort();
        return;
      }
      if (running) return;
      const elapsed = Date.now() - lastRunAt;
      if (elapsed >= pollIntervalMs) {
        void poll();
      } else {
        schedule(pollIntervalMs - elapsed);
      }
    };

    // 整体看护上限：链接 TTL + 5s 宽限。expiresIn 缺省按后端默认 600s。
    const ttlSeconds = typeof expiresIn === "number" && expiresIn > 0 ? expiresIn : 600;
    deadlineTimer = setTimeout(() => {
      if (stopped) return;
      stop();
      optionsRef.current.onTimeout?.();
    }, ttlSeconds * 1000 + 5000);

    document.addEventListener("visibilitychange", handleVisibility);
    void poll();

    return () => {
      document.removeEventListener("visibilitychange", handleVisibility);
      stop();
    };
  }, [linkId, secret, scene, expiresIn, enabled, pollIntervalMs]);
}
