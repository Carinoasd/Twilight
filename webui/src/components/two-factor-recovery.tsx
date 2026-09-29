"use client";

import { create } from "zustand";
import { useState } from "react";
import { useI18n } from "@/lib/i18n";
import { Dialog, DialogContent, DialogDescription, DialogHeader, DialogTitle } from "@/components/ui/dialog";
import { Button } from "@/components/ui/button";

// Memory only, above route layouts so session revocation cannot unmount codes.
export const useRecoveryDisplay = create<{ codes: string[]; show: (codes: string[]) => void; clear: () => void }>(set => ({ codes: [], show: codes => set({ codes }), clear: () => set({ codes: [] }) }));

export function TwoFactorRecoveryDisplay() {
  const { t } = useI18n();
  const { codes, clear } = useRecoveryDisplay();
  const [saved, setSaved] = useState(false);
  const [message, setMessage] = useState("");
  const close = () => { if (saved) { clear(); setSaved(false); setMessage(""); } };
  const copy = async () => { try { await navigator.clipboard.writeText(codes.join("\n")); setMessage(t("twoFactor.copied")); } catch { setMessage(t("twoFactor.copyFailed")); } };
  const download = () => {
    const url = URL.createObjectURL(new Blob([codes.join("\n") + "\n"], { type: "text/plain;charset=utf-8" }));
    const a = document.createElement("a"); a.href = url; a.download = "twilight-recovery-codes.txt"; a.click(); setTimeout(() => URL.revokeObjectURL(url), 1000);
  };
  return <Dialog open={codes.length > 0} onOpenChange={open => { if (!open) close(); }}>
    <DialogContent className="[&>button:last-child]:hidden" onEscapeKeyDown={e => e.preventDefault()} onInteractOutside={e => e.preventDefault()}>
      <DialogHeader><DialogTitle>{t("twoFactor.saveCodes")}</DialogTitle><DialogDescription>{t("twoFactor.saveHint")}</DialogDescription></DialogHeader>
      <pre className="select-all whitespace-pre-wrap break-all rounded-lg bg-muted p-3 text-xs leading-6">{codes.join("\n")}</pre>
      <div className="flex flex-wrap gap-2"><Button variant="outline" onClick={() => void copy()}>{t("twoFactor.copy")}</Button><Button variant="outline" onClick={download}>{t("twoFactor.download")}</Button></div>
      {message && <p role="status" className="text-sm">{message}</p>}
      <label className="flex items-start gap-2 text-sm"><input type="checkbox" className="mt-1" checked={saved} onChange={e => setSaved(e.target.checked)} />{t("twoFactor.saved")}</label>
      <Button disabled={!saved} onClick={close}>{t("twoFactor.backToLogin")}</Button>
    </DialogContent>
  </Dialog>;
}
