"use client";

import { useCallback, useEffect, useRef, useState } from "react";
import { useRouter } from "next/navigation";
import { Loader2, Copy, Bot, Check, AlertCircle, Send } from "lucide-react";
import { Card, CardContent, CardDescription, CardHeader, CardTitle } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { useToast } from "@/hooks/use-toast";
import { useTelegramLinkStatus } from "@/hooks/use-telegram-link-status";
import { useAuthStore } from "@/store/auth";
import { useSystemStore } from "@/store/system";
import { useI18n } from "@/lib/i18n";
import { api } from "@/lib/api";
import type { TelegramLinkIssue } from "@/lib/api-types-v2";
import { telegramBotUrl } from "@/lib/safe-url";

export default function RebindGuard() {
  const { toast } = useToast();
  const { t } = useI18n();
  const router = useRouter();
  const { user, fetchUser } = useAuthStore();
  const { info: systemInfo } = useSystemStore();
  const [telegramLink, setTelegramLink] = useState<TelegramLinkIssue | null>(null);
  const [isLoading, setIsLoading] = useState(false);
  const [isBound, setIsBound] = useState(false);
  const completeRef = useRef(false);

  const botUsername = systemInfo?.telegram_bot?.username;
  const botUrl = telegramBotUrl(botUsername, systemInfo?.telegram_bot?.url);

  const completeRebind = useCallback(async () => {
    if (completeRef.current) return;
    completeRef.current = true;
    try {
      const res = await api.completeRebind();
      if (res.success) {
        await fetchUser();
        router.replace("/dashboard");
      }
    } catch (error: any) {
      completeRef.current = false;
      if (error?.message) {
        toast({ title: t("settings.rebindCompleteFailed"), description: error.message, variant: "destructive" });
      }
    }
  }, [fetchUser, router, t, toast]);

  // 新 Telegram 已经绑上（例如在 Bot 里确认后关了网页，再回来）：服务端通常已自动
  // 结束换绑；若因资格校验等原因还没结束，这里补一次，避免卡在“请获取绑定链接”
  // 却又因为“已绑定”无法签发新链接。
  useEffect(() => {
    if (user?.rebinding_in_progress && user?.telegram_id) {
      setIsBound(true);
      void completeRebind();
    }
  }, [user?.rebinding_in_progress, user?.telegram_id, completeRebind]);

  // 统一的绑定链接状态轮询：带超时中断 + 请求中断，绑定成功即收尾换绑。
  useTelegramLinkStatus({
    linkId: telegramLink?.link_id ?? null,
    scene: "user",
    expiresIn: telegramLink?.expires_in,
    enabled: Boolean(telegramLink) && !isBound,
    onBound: () => {
      setIsBound(true);
      setTimeout(() => void completeRebind(), 1500);
    },
    onTerminalError: (data) => {
      setTelegramLink(null);
      toast({ title: t("settings.getBindCodeFailed"), description: data.message, variant: "destructive" });
    },
    onTimeout: () => {
      setTelegramLink(null);
      toast({ title: t("settings.getBindCodeFailed"), description: t("settings.retryBindCode"), variant: "destructive" });
    },
  });

  const handleGetBindCode = async () => {
    setIsLoading(true);
    setTelegramLink(null);
    try {
      const res = await api.createTelegramLink();
      if (res.success && res.data?.link_id) {
        setTelegramLink(res.data);
        toast({
          title: t("settings.bindCodeGenerated"),
          variant: "success",
        });
      } else {
        toast({ title: t("settings.getBindCodeFailed"), description: res.message, variant: "destructive" });
      }
    } catch (error: any) {
      toast({ title: t("settings.getBindCodeFailed"), description: error.message, variant: "destructive" });
    } finally {
      setIsLoading(false);
    }
  };

  return (
    <div className="flex min-h-screen items-center justify-center bg-background p-4">
      <div className="w-full max-w-lg animate-in fade-in slide-in-from-bottom-2 duration-300">
        <Card className="border-primary/20 bg-card/90 backdrop-blur-sm">
          <CardHeader className="text-center pb-2">
            <div className="mx-auto mb-3 flex h-14 w-14 items-center justify-center rounded-full bg-primary/10">
              {isBound ? (
                <Check className="h-7 w-7 text-emerald-500" />
              ) : telegramLink ? (
                <Loader2 className="h-7 w-7 animate-spin text-primary" />
              ) : (
                <Bot className="h-7 w-7 text-primary" />
              )}
            </div>
            <CardTitle className="text-xl">
              {isBound ? t("settings.rebindCompleteTitle") : t("settings.rebindRequiredTitle")}
            </CardTitle>
            <CardDescription className="text-sm">
              {isBound
                ? t("settings.rebindCompleteDescription")
                : t("settings.rebindRequiredDescription")}
            </CardDescription>
          </CardHeader>
          <CardContent className="space-y-4">
            {!isBound && !telegramLink && (
              <div className="text-center space-y-3">
                <p className="text-sm text-muted-foreground">
                  {t("settings.rebindInstructions")}
                </p>
                <Button
                  className="w-full"
                  onClick={handleGetBindCode}
                  disabled={isLoading}
                  size="lg"
                >
                  {isLoading ? (
                    <Loader2 className="mr-2 h-5 w-5 animate-spin" />
                  ) : (
                    <Bot className="mr-2 h-5 w-5" />
                  )}
                  {t("settings.getBindCode")}
                </Button>
              </div>
            )}

            {telegramLink && !isBound && (
              <div className="space-y-3">
                {telegramLink.deep_link && (
                  <Button className="w-full" size="lg" asChild>
                    <a href={telegramLink.deep_link} target="_blank" rel="noopener noreferrer">
                      <Send className="mr-2 h-5 w-5" />
                      {t("settings.openTelegramLink", { username: telegramLink.bot_username })}
                    </a>
                  </Button>
                )}
                <div className="rounded-lg bg-primary/5 p-4 text-center">
                  <p className="text-sm text-muted-foreground mb-2">
                    {t("settings.sendBindWithin", { minutes: Math.floor((telegramLink.expires_in ?? 0) / 60), bot: telegramLink.bot_username ? `@${telegramLink.bot_username}` : botUsername ? `@${botUsername}` : "Telegram Bot" })}
                  </p>
                  <code className="block min-w-0 max-w-full break-all font-mono text-sm font-semibold tracking-wide text-primary">
                    {telegramLink.manual_command}
                  </code>
                </div>

                <div className="flex gap-2">
                  <Button
                    variant="outline"
                    className="flex-1"
                    onClick={() => {
                      navigator.clipboard.writeText(telegramLink.manual_command);
                      toast({ title: t("settings.copyCommand"), variant: "success" });
                    }}
                  >
                    <Copy className="mr-2 h-4 w-4" />
                    {t("settings.copyCommand")}
                  </Button>
                  {botUrl && !telegramLink.deep_link && (
                    <Button variant="outline" className="flex-1" asChild>
                      <a href={botUrl} target="_blank" rel="noopener noreferrer">
                        <Bot className="mr-2 h-4 w-4" />
                        {t("settings.openBot")}
                      </a>
                    </Button>
                  )}
                </div>

                <div className="flex items-center justify-center gap-2 text-sm text-muted-foreground">
                  <Loader2 className="h-4 w-4 animate-spin" />
                  {t("settings.waitingForBind")}
                </div>

                <Button
                  variant="ghost"
                  className="w-full text-xs"
                  onClick={() => {
                    setTelegramLink(null);
                  }}
                >
                  {t("settings.retryBindCode")}
                </Button>
              </div>
            )}

            {isBound && (
              <div className="text-center space-y-3">
                <AlertCircle className="mx-auto h-6 w-6 text-emerald-500" />
                <p className="text-sm text-muted-foreground">
                  {t("settings.rebindRedirecting")}
                </p>
              </div>
            )}
          </CardContent>
        </Card>
      </div>
    </div>
  );
}
