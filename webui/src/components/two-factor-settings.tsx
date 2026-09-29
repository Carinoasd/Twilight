"use client";

import { useEffect, useRef, useState } from "react";
import { QRCodeSVG } from "qrcode.react";
import { api } from "@/lib/api";
import { useI18n } from "@/lib/i18n";
import { endRevokedSession } from "@/store/auth";
import { TwoFactorSetup, TwoFactorStatus, twoFactorErrorKey } from "@/lib/two-factor";
import { useRecoveryDisplay } from "@/components/two-factor-recovery";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";

export function TwoFactorSettings() {
  const { t } = useI18n();
  const [status, setStatus] = useState<TwoFactorStatus | null>(null);
  const [setup, setSetup] = useState<TwoFactorSetup | null>(null);
  const [password, setPassword] = useState("");
  const [code, setCode] = useState("");
  const [recovery, setRecovery] = useState(false);
  const [busy, setBusy] = useState(false);
  const [error, setError] = useState("");
  const [seconds, setSeconds] = useState(0);
  const [operation, setOperation] = useState<"disable" | "regenerate" | null>(null);
  const [reload, setReload] = useState(0);
  const active = useRef(true);
  const controller = useRef<AbortController | null>(null);
  useEffect(() => {
    active.current = true; const ac = new AbortController();
    void api.twoFactorStatus(ac.signal).then(res => { if (!ac.signal.aborted && res.success && res.data) { setStatus(res.data); setError(""); } }).catch(() => { if (!ac.signal.aborted) setError(t("twoFactor.failed")); });
    return () => { active.current = false; ac.abort(); controller.current?.abort(); };
  }, [reload, t]);
  useEffect(() => {
    if (!setup) return;
    const tick = () => { const left = Math.max(0, Math.ceil(setup.expires_at - Date.now() / 1000)); setSeconds(left); if (!left) { setSetup(null); setCode(""); setError(t("twoFactor.expired")); } };
    tick(); const timer = setInterval(tick, 1000); return () => clearInterval(timer);
  }, [setup, t]);
  const run = async (action: "setup" | "enable" | "disable" | "regenerate") => {
    if (busy) return; setBusy(true); setError("");
    const ac = new AbortController(); controller.current = ac;
    try {
      if (action === "setup") {
        const res = await api.setupTwoFactor(password, ac.signal);
        if (ac.signal.aborted || !active.current) return;
        if (!res.success || !res.data) throw new Error();
        setSetup(res.data); setPassword("");
      } else {
        const res = action === "enable"
          ? await api.enableTwoFactor(setup!.request, code.trim(), ac.signal)
          : await api.changeTwoFactor(action === "disable", password, code.trim(), recovery, ac.signal);
        if (ac.signal.aborted || !active.current) return;
        if (!res.success || !res.data) throw new Error();
        setSetup(null); setPassword(""); setCode("");
        if (res.data.recovery_codes?.length) useRecoveryDisplay.getState().show(res.data.recovery_codes);
        endRevokedSession();
      }
    } catch (err) { if (!ac.signal.aborted && active.current) setError(t(twoFactorErrorKey((err as { errorCode?: string }).errorCode))); }
    finally { if (controller.current === ac) controller.current = null; if (active.current) setBusy(false); }
  };
  const cancel = () => { if (setup) void api.cancelTwoFactor(setup.request).catch(() => {}); setSetup(null); setCode(""); setPassword(""); setOperation(null); setError(""); };
  return <Card>
    <CardHeader><CardTitle>{t("twoFactor.title")}</CardTitle><CardDescription>{t("twoFactor.settingsHint")}</CardDescription></CardHeader>
    <CardContent className="space-y-4">
      {error && <p role="alert" className="text-sm text-destructive">{error}</p>}
      {!status ? <Button variant="outline" onClick={() => setReload(reload + 1)}>{t("common.refresh")}</Button> : <>
        <p className="text-sm font-medium">{t(status.enabled ? "twoFactor.enabled" : "twoFactor.disabled")}</p>
        {status.enabled && <p className="text-sm text-muted-foreground">{t("twoFactor.remaining", { count: status.recovery_remaining })} · {new Date(status.enabled_at * 1000).toLocaleDateString()}</p>}
        {!status.key_ready && <p className="text-sm text-muted-foreground">{t("twoFactor.keyMissing")}</p>}
        {!status.enrollment_allowed && <p className="text-sm text-muted-foreground">{t("twoFactor.enrollmentClosed")}</p>}
        {setup ? <form className="space-y-3" onSubmit={e => { e.preventDefault(); void run("enable"); }}>
          <p className="text-sm">{t("twoFactor.scanHint")}</p>
          <div className="w-fit max-w-full rounded-lg bg-white p-3"><QRCodeSVG value={setup.uri} size={208} marginSize={4} title={t("twoFactor.qrLabel")} style={{ maxWidth: "100%", height: "auto" }} /></div>
          <p className="text-sm">{t("twoFactor.manualKey")}</p><code className="block select-all break-all rounded bg-muted p-2 text-sm">{setup.secret}</code>
          <Label htmlFor="factor-setup-code">{t("twoFactor.code")}</Label><Input id="factor-setup-code" autoComplete="one-time-code" inputMode="numeric" maxLength={6} value={code} onChange={e => setCode(e.target.value)} disabled={busy} />
          <p className="text-sm">{t("twoFactor.expires", { seconds })}</p>
          <div className="flex flex-wrap gap-2"><Button disabled={busy || code.length !== 6 || !seconds}>{t("twoFactor.enable")}</Button><Button type="button" variant="ghost" disabled={busy} onClick={cancel}>{t("common.cancel")}</Button></div>
        </form> : (status.enabled && !operation) ? <div className="flex flex-wrap gap-2">
          <Button variant="outline" disabled={!status.key_ready} onClick={() => setOperation("regenerate")}>{t("twoFactor.regenerate")}</Button>
          <Button variant="outline" disabled={!status.key_ready} onClick={() => setOperation("disable")}>{t("twoFactor.disable")}</Button>
        </div> : status.key_ready && (status.enabled || status.enrollment_allowed) ? <form className="space-y-3" onSubmit={e => { e.preventDefault(); void run(operation || "setup"); }}>
          {operation && <p className="text-sm">{t(operation === "disable" ? "twoFactor.disableHint" : "twoFactor.regenerateHint")}</p>}
          <Label htmlFor="factor-password">{t("twoFactor.password")}</Label><Input id="factor-password" type="password" autoComplete="current-password" value={password} onChange={e => setPassword(e.target.value)} disabled={busy} />
          {operation && <><Label htmlFor="factor-change-code">{t(recovery ? "twoFactor.recoveryCode" : "twoFactor.code")}</Label><Input id="factor-change-code" autoComplete="one-time-code" inputMode={recovery ? "text" : "numeric"} maxLength={recovery ? 40 : 6} value={code} onChange={e => setCode(e.target.value)} disabled={busy} /><Button type="button" variant="ghost" disabled={busy} onClick={() => { setRecovery(!recovery); setCode(""); }}>{t(recovery ? "twoFactor.useAuthenticator" : "twoFactor.useRecovery")}</Button></>}
          <div className="flex flex-wrap gap-2"><Button disabled={busy || !password || (!!operation && !code.trim())}>{t(busy ? "twoFactor.verifying" : operation === "disable" ? "twoFactor.disable" : operation === "regenerate" ? "twoFactor.regenerate" : "twoFactor.start")}</Button>{operation && <Button type="button" variant="ghost" disabled={busy} onClick={cancel}>{t("common.cancel")}</Button>}</div>
        </form> : null}
      </>}
    </CardContent>
  </Card>;
}
