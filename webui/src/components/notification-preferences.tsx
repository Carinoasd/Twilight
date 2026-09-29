"use client";

import { useState } from "react";
import { Bell, Loader2 } from "lucide-react";
import { api, type UserSettings } from "@/lib/api";
import { useI18n } from "@/lib/i18n";
import { useToast } from "@/hooks/use-toast";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";

const rows = [
  { key: "notify_on_login_telegram", label: "loginNotifyTelegram", description: "loginNotifyTelegramDesc", channel: "telegram" },
  { key: "notify_on_login_email", label: "loginNotifyEmail", description: "loginNotifyEmailDesc", channel: "email" },
  { key: "notify_on_ticket_telegram", label: "ticketNotifyTelegram", description: "ticketNotifyTelegramDesc", channel: "telegram" },
  { key: "notify_on_ticket_email", label: "ticketNotifyEmail", description: "ticketNotifyEmailDesc", channel: "email" },
  { key: "notify_on_expiry_telegram", label: "expiryNotifyTelegram", description: "expiryNotifyTelegramDesc", channel: "telegram" },
  { key: "notify_on_expiry_email", label: "expiryNotifyEmail", description: "expiryNotifyEmailDesc", channel: "email" },
] as const;
type PreferenceKey = (typeof rows)[number]["key"];

export function NotificationPreferences({ settings, telegramBound, emailVerified, onSaved }: {
  settings: UserSettings;
  telegramBound: boolean;
  emailVerified: boolean;
  onSaved: (patch: Partial<UserSettings>) => void;
}) {
  const { t } = useI18n();
  const { toast } = useToast();
  const [saving, setSaving] = useState<PreferenceKey | null>(null);
  const save = async (key: PreferenceKey, value: boolean) => {
    if (saving) return;
    setSaving(key);
    try {
      const result = await api.updateMySettings({ [key]: value });
      if (!result.success) throw new Error("notification preference rejected");
      onSaved({ [key]: value });
    } catch {
      toast({ title: t("settings.notificationSaveFailed"), variant: "destructive" });
    } finally {
      setSaving(null);
    }
  };
  return (
    <Card className="glass-card">
      <CardHeader>
        <CardTitle className="flex items-center gap-2"><Bell className="h-5 w-5" />{t("settings.notificationsTitle")}</CardTitle>
        <CardDescription>{t("settings.notificationsDescription")}</CardDescription>
      </CardHeader>
      <CardContent className="divide-y">
        {rows.map(({ key, label, description, channel }) => {
          const checked = settings[key] ?? (key === "notify_on_expiry_telegram");
          const bound = channel === "telegram" ? telegramBound : emailVerified;
          const enabled = settings.notification_channels?.[key] ?? true;
          const hint = !enabled ? "notificationAdminDisabled" : !bound ? (channel === "telegram" ? "notificationBindTelegram" : "notificationVerifyEmail") : null;
          return (
            <div key={key} className="flex items-start justify-between gap-4 py-4 first:pt-0 last:pb-0">
              <div className="min-w-0 space-y-1">
                <Label htmlFor={key}>{t(`settings.${label}`)}</Label>
                <p id={`${key}-hint`} className="text-sm text-muted-foreground">{t(`settings.${description}`)}{hint && <span className="mt-1 block">{t(`settings.${hint}`)}</span>}</p>
              </div>
              <div className="flex shrink-0 items-center gap-2">
                {saving === key && <Loader2 className="h-4 w-4 animate-spin" aria-label={t("common.saving")} />}
                <Switch id={key} checked={checked} disabled={saving !== null || (!bound && !checked)} aria-describedby={`${key}-hint`} onCheckedChange={(value) => void save(key, value)} />
              </div>
            </div>
          );
        })}
      </CardContent>
    </Card>
  );
}
