"use client";

import { useEffect, useState } from "react";
import { QRCodeSVG } from "qrcode.react";
import { useI18n } from "@/lib/i18n";

export function telegramQRUrl(value: string): string | null {
  try {
    const u = new URL(value);
    return u.protocol === "https:" && u.host === "t.me" && !u.username && !u.password
      && /^\/[A-Za-z0-9_]{5,32}$/.test(u.pathname)
      && /^[A-Za-z0-9_-]{8,64}$/.test(u.searchParams.get("start") || "")
      && [...u.searchParams.keys()].length === 1 && !u.hash ? u.href : null;
  } catch { return null; }
}

/** The token stays in this page; QR generation makes no external request. */
export function TelegramQR({ url, expiresIn = 600 }: { url: string; expiresIn?: number }) {
  const { t } = useI18n();
  const [expired, setExpired] = useState(false);
  useEffect(() => {
    setExpired(false);
    const timer = setTimeout(() => setExpired(true), Math.max(0, expiresIn) * 1000);
    return () => clearTimeout(timer);
  }, [url, expiresIn]);
  const safe = telegramQRUrl(url);
  if (!safe || expired) return null;
  return <figure className="flex min-w-0 flex-col items-center gap-2">
    <QRCodeSVG value={safe} size={208} marginSize={4} level="M" bgColor="#ffffff" fgColor="#000000" title={t("telegramQR.imageAlt")} className="h-auto max-w-full rounded-lg" />
    <figcaption className="text-center text-xs text-muted-foreground">{t("telegramQR.scanHint")}</figcaption>
  </figure>;
}
