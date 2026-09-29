"use client";

import { useEffect, useState } from "react";
import { api } from "@/lib/api";
import { useI18n } from "@/lib/i18n";

export function TwoFactorReadiness() {
  const { t } = useI18n();
  const [ready, setReady] = useState<boolean | null>(null);
  useEffect(() => {
    const controller = new AbortController();
    void api.twoFactorStatus(controller.signal).then(res => { if (!controller.signal.aborted && res.success && res.data) setReady(res.data.key_ready); }).catch(() => {});
    return () => controller.abort();
  }, []);
  return <p className="rounded-lg border p-3 text-sm text-muted-foreground">{t(ready === true ? "twoFactor.adminReady" : ready === false ? "twoFactor.adminNotReady" : "twoFactor.adminUnknown")}</p>;
}
