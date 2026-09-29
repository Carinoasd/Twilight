"use client";

import { useEffect, useRef, useState } from "react";
import { api } from "@/lib/api";
import { useAuthStore } from "@/store/auth";
import { useI18n } from "@/lib/i18n";
import { TwoFactorChallenge, twoFactorErrorKey } from "@/lib/two-factor";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

export function TwoFactorLogin({ challenge, onSuccess, onCancel }: { challenge: TwoFactorChallenge; onSuccess: () => void; onCancel: () => void }) {
  const { t } = useI18n();
  const [code, setCode] = useState("");
  const [recovery, setRecovery] = useState(false);
  const [busy, setBusy] = useState(false);
  const [failed, setFailed] = useState(false);
  const [error, setError] = useState("");
  const [seconds, setSeconds] = useState(0);
  const active = useRef(true);
  const completed = useRef(false);
  useEffect(() => {
    active.current = true;
    const tick = () => setSeconds(Math.max(0, Math.ceil(challenge.expires_at - Date.now() / 1000)));
    tick(); const timer = setInterval(tick, 1000);
    return () => { active.current = false; clearInterval(timer); queueMicrotask(() => { if (!active.current && !completed.current) { useAuthStore.getState().cancelAuthentication(); void api.cancelTwoFactor(challenge.request).catch(() => {}); } }); };
  }, [challenge.request, challenge.expires_at]);
  const submit = async (event: React.FormEvent) => {
    event.preventDefault(); if (busy || !seconds || failed) return;
    setBusy(true); setError("");
    const result = await useAuthStore.getState().verifyTwoFactor(challenge.request, code.trim(), recovery);
    if (!active.current) return;
    setBusy(false); setCode("");
    if (result.ok) { completed.current = true; onSuccess(); return; }
    setError(t(twoFactorErrorKey(result.errorCode)));
    // Only a definitive invalid-code response is safe to retry. Network failures
    // may have consumed a recovery code; start a fresh primary login in that case.
    if (result.errorCode !== "AUTH_TWO_FACTOR_CODE_INVALID") setFailed(true);
  };
  return <section className="space-y-4 rounded-xl border p-4" aria-label={t("twoFactor.title")}>
    <h2 className="text-lg font-semibold">{t("twoFactor.loginTitle")}</h2>
    <p className="text-sm text-muted-foreground">{t("twoFactor.loginHint")}</p>
    <form onSubmit={submit} className="space-y-3">
      <Label htmlFor="two-factor-code">{t(recovery ? "twoFactor.recoveryCode" : "twoFactor.code")}</Label>
      <Input id="two-factor-code" value={code} onChange={e => setCode(e.target.value)} autoComplete="one-time-code" inputMode={recovery ? "text" : "numeric"} maxLength={recovery ? 40 : 6} disabled={busy || !seconds || failed} autoFocus />
      <p role="status" className="text-sm">{seconds ? t("twoFactor.expires", { seconds }) : t("twoFactor.expired")}</p>
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      <Button className="w-full" disabled={busy || !seconds || failed || !code.trim()}>{t(busy ? "twoFactor.verifying" : "twoFactor.verify")}</Button>
    </form>
    <div className="flex flex-wrap gap-2">
      <Button type="button" variant="outline" disabled={busy} onClick={() => { setRecovery(!recovery); setCode(""); }}>{t(recovery ? "twoFactor.useAuthenticator" : "twoFactor.useRecovery")}</Button>
      <Button type="button" variant="ghost" disabled={busy} onClick={onCancel}>{t("common.cancel")}</Button>
    </div>
  </section>;
}
