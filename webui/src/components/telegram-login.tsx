"use client";

import { useEffect, useRef, useState } from "react";
import { api } from "@/lib/api";
import { useAuthStore } from "@/store/auth";
import { useI18n } from "@/lib/i18n";
import { Button } from "@/components/ui/button";
import { TelegramQR, telegramQRUrl } from "@/components/telegram-qr";

type Request = { id: string; secret: string; deep_link: string; check_code: string; expires_in: number; deadline: number };
type Status = "idle" | "creating" | "pending" | "scanned" | "consuming" | "expired" | "rejected" | "failed" | "done";

export function TelegramLogin({ onSuccess, onBusyChange, disabled }: { onSuccess: () => void; onBusyChange: (busy: boolean) => void; disabled: boolean }) {
  const { t } = useI18n();
  const [request, setRequest] = useState<Request | null>(null);
  const [status, setStatus] = useState<Status>("idle");
  const [networkError, setNetworkError] = useState(false);
  const [seconds, setSeconds] = useState(0);
  const mounted = useRef(true);
  const active = useRef<Request | null>(null);
  const controller = useRef<AbortController | null>(null);
  const redeeming = useRef(false);
  const callbacks = useRef({ onSuccess, onBusyChange });
  callbacks.current = { onSuccess, onBusyChange };

  useEffect(() => {
    mounted.current = true;
    return () => {
    mounted.current = false;
    controller.current?.abort();
    if (active.current && !redeeming.current) void api.cancelTelegramLogin(active.current.id, active.current.secret).catch(() => {});
    active.current = null;
    };
  }, []);

  const clear = () => {
    controller.current?.abort();
    const old = active.current;
    active.current = null;
    setRequest(null);
    setStatus("idle");
    callbacks.current.onBusyChange(false);
    if (old) void api.cancelTelegramLogin(old.id, old.secret).catch(() => {});
  };

  const create = async () => {
    if (controller.current || redeeming.current) return;
    setStatus("creating"); setNetworkError(false);
    callbacks.current.onBusyChange(true);
    const ac = new AbortController(); controller.current = ac;
    const started = Date.now();
    try {
      const res = await api.createTelegramLogin(ac.signal);
      if (!mounted.current || ac.signal.aborted) return;
      if (!res.success || !res.data || !telegramQRUrl(res.data.deep_link)) throw new Error("unavailable");
      const next = { ...res.data, deadline: started + res.data.expires_in * 1000 };
      active.current = next; setRequest(next); setStatus("pending");
    } catch {
      if (mounted.current && !ac.signal.aborted) { setStatus("failed"); callbacks.current.onBusyChange(false); }
    } finally { if (controller.current === ac) controller.current = null; }
  };

  useEffect(() => {
    if (!request) return;
    let stopped = false;
    let pollTimer: ReturnType<typeof setTimeout> | undefined;
    let polling = false;
    const stop = (state: Status) => {
      stopped = true; clearTimeout(pollTimer); controller.current?.abort(); controller.current = null;
      active.current = null; setRequest(null); setStatus(state); callbacks.current.onBusyChange(false);
    };
    const tick = () => {
      const left = Math.max(0, Math.ceil((request.deadline - Date.now()) / 1000));
      setSeconds(left);
      if (!left && !stopped && !redeeming.current) stop("expired");
    };
    const poll = async () => {
      if (stopped || polling || document.visibilityState !== "visible" || redeeming.current) return;
      tick(); if (stopped) return;
      polling = true;
      const ac = new AbortController(); controller.current = ac;
      try {
        const res = await api.getTelegramLogin(request.id, request.secret, ac.signal);
        if (stopped || ac.signal.aborted) return;
        if (!res.success || !res.data) throw new Error("unavailable");
        setNetworkError(false);
        const state = res.data.status;
        if (state === "approved") {
          redeeming.current = true; setStatus("consuming");
          const result = await useAuthStore.getState().loginTelegram(request.id, request.secret);
          redeeming.current = false;
          if (stopped || !mounted.current) return;
          stop(result.ok ? "done" : "failed");
          if (result.ok) callbacks.current.onSuccess();
        } else if (state === "pending" || state === "scanned") setStatus(state);
        else stop(state === "rejected" ? "rejected" : "expired");
      } catch (e) {
        if (!stopped && !ac.signal.aborted) {
          const code = (e as { errorCode?: string }).errorCode;
          if (code === "TG_LOGIN_UNAVAILABLE" || code === "TG_LOGIN_INVALID") stop("expired");
          else setNetworkError(true);
        }
      } finally {
        polling = false;
        if (controller.current === ac) controller.current = null;
        if (!stopped && document.visibilityState === "visible") pollTimer = setTimeout(poll, 3000);
      }
    };
    const visible = () => {
      clearTimeout(pollTimer);
      if (document.visibilityState === "visible") { tick(); void poll(); }
      else if (!redeeming.current) controller.current?.abort();
    };
    tick(); void poll();
    const timer = setInterval(tick, 1000);
    document.addEventListener("visibilitychange", visible);
    return () => { stopped = true; clearTimeout(pollTimer); clearInterval(timer); document.removeEventListener("visibilitychange", visible); controller.current?.abort(); controller.current = null; };
  }, [request]);

  return <section className="space-y-3 rounded-xl border p-4 text-center" aria-label={t("telegramQR.loginTitle")}>
    {!request ? <>
      {status !== "idle" && <p role="status" className="text-sm">{t(`telegramQR.${status}`)}</p>}
      <Button type="button" variant="outline" className="w-full" disabled={disabled || status === "creating"} onClick={() => void create()}>{t("telegramQR.loginTitle")}</Button>
    </> : <>
      {(status === "pending" || status === "scanned") && <TelegramQR key={request.id} url={request.deep_link} expiresIn={Math.max(0, (request.deadline - Date.now()) / 1000)} />}
      <p className="text-sm font-medium">{t("telegramQR.checkCode", { code: request.check_code })}</p>
      <p className="text-xs text-muted-foreground">{t("telegramQR.safetyHint")}</p>
      <p role="status" className="text-sm">{t(`telegramQR.${status}`)} · {t("telegramQR.expires", { seconds })}</p>
      {networkError && <p role="alert" className="text-sm text-destructive">{t("telegramQR.networkError")}</p>}
      <div className="flex flex-wrap justify-center gap-2">
        {status !== "consuming" && <Button asChild variant="outline"><a href={telegramQRUrl(request.deep_link) || undefined} target="_blank" rel="noopener noreferrer">{t("telegramQR.openBot")}</a></Button>}
        <Button type="button" variant="ghost" disabled={status === "consuming"} onClick={clear}>{t("common.cancel")}</Button>
      </div>
    </>}
  </section>;
}
