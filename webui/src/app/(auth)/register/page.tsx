"use client";

import { useEffect, useState } from "react";
import Link from "next/link";
import { useRouter } from "next/navigation";
import {
  Eye,
  EyeOff,
  Loader2,
  ShieldPlus,
  UserPlus,
  Bot,
  Send,
  ArrowLeft,
  ArrowRight,
} from "lucide-react";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { useTelegramLinkStatus } from "@/hooks/use-telegram-link-status";
import { useToast } from "@/hooks/use-toast";
import { api, type RegisterAvailability, type RegisterData } from "@/lib/api";
import type { TelegramLinkIssue } from "@/lib/api-types-v2";
import { ApiError } from "@/lib/api-request";
import { ErrCodes } from "@/lib/errcode";
import { useSystemStore } from "@/store/system";
import { passwordStrengthLabel, validatePasswordStrength } from "@/lib/password";
import { friendlyError, validateUsername } from "@/lib/validators";
import { sanitizeExternalUrl, telegramBotUrl } from "@/lib/safe-url";
import { useI18n } from "@/lib/i18n";
import { AuthBrand, AuthStepDots, AUTH_PRIMARY_BTN } from "../auth-ui";

export default function RegisterPage() {
  const router = useRouter();
  const { toast } = useToast();
  const { t } = useI18n();
  const { info: systemInfo } = useSystemStore();

  // --- Account form state ---
  const [formData, setFormData] = useState({
    username: "",
    password: "",
    confirmPassword: "",
    email: "",
    regCode: "",
  });
  const [showPassword, setShowPassword] = useState(false);
  const [registerAvailability, setRegisterAvailability] =
    useState<RegisterAvailability | null>(null);

  // --- Telegram binding state ---
  const [telegramLink, setTelegramLink] = useState<TelegramLinkIssue | null>(null);
  const [bindConfirmed, setBindConfirmed] = useState(false);
  const [isBindCodeLoading, setIsBindCodeLoading] = useState(false);

  // --- Submission ---
  const [isRegisterLoading, setIsRegisterLoading] = useState(false);

  // --- Wizard ---
  const hasTelegramStep = Boolean(
    systemInfo?.features?.force_bind_telegram || systemInfo?.features?.telegram,
  );
  const forceBindTelegram = Boolean(systemInfo?.features?.force_bind_telegram);
  const emailEnabled = Boolean(systemInfo?.features?.email_enabled);
  const TOTAL_STEPS = hasTelegramStep ? 2 : 1;
  const [step, setStep] = useState(0);

  // Derived
  const registerRequiresCode = Boolean(
    registerAvailability?.requires_reg_code && (registerAvailability?.current_users ?? 0) > 0,
  );
  const canRegister =
    registerAvailability?.can_register ?? registerAvailability?.available ?? true;

  // Telegram links
  const requiredTelegramLinks = [
    ...(systemInfo?.required_telegram_links?.groups || []),
    ...(systemInfo?.required_telegram_links?.channels || []),
  ];
  const telegramLinks = [
    ...(requiredTelegramLinks.length > 0
      ? requiredTelegramLinks
      : [
          ...(systemInfo?.telegram_links?.groups || []),
          ...(systemInfo?.telegram_links?.channels || []),
        ]),
  ]
    .map((item) => ({ ...item, url: sanitizeExternalUrl(item.url) }))
    .filter((item): item is { label: string; url: string } => Boolean(item.url));
  const botUsername = systemInfo?.telegram_bot?.username;
  const botUrl = telegramBotUrl(systemInfo?.telegram_bot?.username, systemInfo?.telegram_bot?.url);

  // Init
  useEffect(() => {
    void refreshRegisterAvailability();
  }, []);

  const handleChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    setFormData((p) => ({ ...p, [e.target.name]: e.target.value }));
  };

  const refreshRegisterAvailability = async () => {
    try {
      const res = await api.getRegisterAvailability();
      if (res.success && res.data) setRegisterAvailability(res.data);
    } catch {
      // ignore
    }
  };

  // ---- Telegram binding: deep link + secret-backed status polling ----

  const handleCreateTelegramLink = async () => {
    setIsBindCodeLoading(true);
    try {
      const res = await api.createRegisterTelegramLink();
      if (!res.success || !res.data?.link_id) {
        throw new Error(res.message || t("auth.register.bindCodeFailedDescription"));
      }
      setTelegramLink(res.data);
      setBindConfirmed(false);
      toast({
        title: t("auth.register.bindCodeGenerated"),
        description: t("auth.register.bindCodeGeneratedDescription"),
        variant: "success",
      });
    } catch (error: any) {
      toast({
        title: t("auth.register.bindCodeFailed"),
        description: error.message || t("auth.register.bindCodeFailedDescription"),
        variant: "destructive",
      });
    } finally {
      setIsBindCodeLoading(false);
    }
  };

  useTelegramLinkStatus({
    linkId: telegramLink?.link_id ?? null,
    secret: telegramLink?.link_secret ?? "",
    scene: "register",
    expiresIn: telegramLink?.expires_in,
    enabled: Boolean(telegramLink) && !bindConfirmed,
    onBound: () => {
      setBindConfirmed(true);
      toast({ title: t("auth.register.telegramBound"), variant: "success" });
    },
    onTerminalError: (data) => {
      setTelegramLink(null);
      setBindConfirmed(false);
      toast({ title: t("auth.register.telegramIncomplete"), description: friendlyError(data.error_code, data.message) || t("auth.register.retryBindCode"), variant: "destructive" });
    },
    onTimeout: () => {
      setTelegramLink(null);
      setBindConfirmed(false);
      toast({ title: t("auth.register.telegramIncomplete"), description: t("auth.register.retryBindCode"), variant: "destructive" });
    },
  });

  const refreshBindConfirmedBeforeSubmit = async (): Promise<boolean> => {
    if (!telegramLink) return false;
    try {
      const res = await api.getRegisterTelegramLinkStatus(telegramLink.link_id, telegramLink.link_secret ?? "");
      if (!res.data?.invalid && (res.data?.status === "confirmed" || res.data?.confirmed)) {
        setBindConfirmed(true);
        return true;
      }
      if (res.data?.terminal && res.data.invalid) {
        const description =
          friendlyError(res.data.error_code, res.data.message) ||
          res.data.message ||
          t("auth.register.retryGetBindCode");
        setTelegramLink(null);
        toast({ title: t("auth.register.telegramIncomplete"), description, variant: "destructive" });
      }
    } catch {
      // network blip → keep current state
    }
    return false;
  };

  const reconcileBindCodeAfterRegisterFailure = async () => {
    if (!telegramLink) return;
    try {
      const res = await api.getRegisterTelegramLinkStatus(telegramLink.link_id, telegramLink.link_secret ?? "");
      const data = res.data;
      if (data?.terminal && data.invalid) {
        setTelegramLink(null);
        setBindConfirmed(false);
      }
    } catch {
      // Keep current UI state on a transient status-check failure.
    }
  };

  // ---- Validation ----

  const validateAccountStep = (): boolean => {
    const uc = validateUsername(formData.username);
    if (!uc.ok) {
      toast({ title: t("auth.register.invalidUsername"), description: uc.message, variant: "destructive" });
      return false;
    }
    if (registerAvailability && (!canRegister || !registerAvailability.available)) {
      toast({
        title: t("auth.register.unavailable"),
        description: registerAvailability.message,
        variant: "destructive",
      });
      return false;
    }
    if (registerRequiresCode && !formData.regCode.trim()) {
      toast({
        title: t("auth.register.regCodeRequired"),
        description: t("auth.register.regCodeRequiredDescription"),
        variant: "destructive",
      });
      return false;
    }
    if (!formData.password) {
      toast({ title: t("auth.register.passwordRequired"), variant: "destructive" });
      return false;
    }
    if (formData.password !== formData.confirmPassword) {
      toast({
        title: t("auth.register.passwordMismatch"),
        description: t("auth.register.passwordMismatchDescription"),
        variant: "destructive",
      });
      return false;
    }
    const strength = validatePasswordStrength(formData.password, t("common.password"));
    if (!strength.ok) {
      toast({ title: t("auth.register.passwordWeak"), description: strength.message, variant: "destructive" });
      return false;
    }
    return true;
  };

  // ---- Navigation ----

  const goNext = () => {
    if (!validateAccountStep()) return;
    // If only 1 step or no Telegram step → submit directly
    if (TOTAL_STEPS === 1) {
      void doSubmit();
      return;
    }
    setStep(1);
  };

  const goBack = () => setStep(0);

  const skipTelegramAndSubmit = () => {
    void doSubmit();
  };

  const handleFinalSubmit = async () => {
    if (telegramLink && !bindConfirmed) {
      const confirmed = await refreshBindConfirmedBeforeSubmit();
      if (!confirmed) {
        toast({
          title: t("auth.register.telegramCompleteBeforeSubmit"),
          description: t("auth.register.sendBindCommand"),
          variant: "destructive",
        });
        return;
      }
    }
    void doSubmit();
  };

  const doSubmit = async () => {
    setIsRegisterLoading(true);
    try {
      const payload: RegisterData = {
        username: formData.username.trim(),
        email: formData.email || undefined,
        telegram_link_id: telegramLink ? telegramLink.link_id : undefined,
        telegram_link_secret: telegramLink ? telegramLink.link_secret : undefined,
        password: formData.password,
        reg_code: registerRequiresCode ? formData.regCode.trim() : undefined,
      };
      const res = await api.register(payload);
      if (!res.success) {
        await reconcileBindCodeAfterRegisterFailure();
        toast({
          title: t("auth.register.failed"),
          description:
            res.error_code === ErrCodes.UsernameTaken
              ? t("auth.register.usernameTaken")
              : res.message,
          variant: "destructive",
        });
        return;
      }
      toast({
        title: t("auth.register.success"),
        description: t("auth.register.successDescription"),
        variant: "success",
      });
      router.push("/login");
    } catch (error: any) {
      const message =
        error instanceof ApiError && error.errorCode === ErrCodes.UsernameTaken
          ? t("auth.register.usernameTaken")
          : error.message || t("common.checkNetwork");
      await reconcileBindCodeAfterRegisterFailure();
      toast({ title: t("auth.register.failed"), description: message, variant: "destructive" });
    } finally {
      setIsRegisterLoading(false);
      void refreshRegisterAvailability();
    }
  };

  // ---- Step 0: Account form ----

  const step0 = (
    <>
      <div className={`grid grid-cols-1 gap-4 ${emailEnabled ? "sm:grid-cols-2" : ""}`}>
        <div className="space-y-2">
          <Label htmlFor="username" className="ml-1">
            {t("auth.register.requiredUsername")}
          </Label>
          <Input
            id="username"
            name="username"
            placeholder="Username"
            value={formData.username}
            onChange={handleChange}
            autoComplete="username"
            className="h-11"
          />
        </div>
        {emailEnabled && (
          <div className="space-y-2">
            <Label htmlFor="email" className="ml-1">
              {t("common.email")}
            </Label>
            <Input
              id="email"
              name="email"
              type="email"
              placeholder="Email (Optional)"
              value={formData.email}
              onChange={handleChange}
              autoComplete="email"
              className="h-11"
            />
          </div>
        )}
      </div>

      <div className="grid grid-cols-1 gap-4 sm:grid-cols-2">
        <div className="space-y-2">
          <Label htmlFor="password" className="ml-1">
            {t("auth.register.passwordLabel")}
          </Label>
          <div className="relative">
            <Input
              id="password"
              name="password"
              type={showPassword ? "text" : "password"}
              placeholder={t("auth.register.passwordPlaceholder")}
              value={formData.password}
              onChange={handleChange}
              autoComplete="new-password"
              className="h-11 pr-10"
            />
            <button
              type="button"
              onClick={() => setShowPassword(!showPassword)}
              className="absolute right-3 top-1/2 -translate-y-1/2 text-muted-foreground hover:text-foreground"
              aria-label={t("common.showPassword")}
            >
              {showPassword ? <EyeOff className="h-4 w-4" /> : <Eye className="h-4 w-4" />}
            </button>
          </div>
          {formData.password &&
            (() => {
              const s = validatePasswordStrength(formData.password, t("common.password"));
              return (
                <p
                  className={`text-xs ${
                    s.ok ? passwordStrengthLabel(s.score).className : "text-destructive"
                  }`}
                >
                  {s.ok
                    ? t("auth.register.passwordStrength", {
                        label: passwordStrengthLabel(s.score).label,
                      })
                    : s.message}
                </p>
              );
            })()}
        </div>
        <div className="space-y-2">
          <Label htmlFor="confirmPassword" className="ml-1">
            {t("auth.register.confirmPassword")}
          </Label>
          <Input
            id="confirmPassword"
            name="confirmPassword"
            type="password"
            placeholder="Confirm Password"
            value={formData.confirmPassword}
            onChange={handleChange}
            autoComplete="new-password"
            className="h-11"
          />
        </div>
      </div>

      {registerRequiresCode && (
        <div className="space-y-2">
          <Label htmlFor="regCode" className="ml-1">
            {t("auth.register.regCodeLabel")}
          </Label>
          <Input
            id="regCode"
            name="regCode"
            placeholder={t("auth.register.regCodePlaceholder")}
            value={formData.regCode}
            onChange={handleChange}
            className="h-11 font-mono"
          />
          <p className="text-xs text-muted-foreground">
            {t("auth.register.regCodeConsumptionHint")}
          </p>
        </div>
      )}

      {registerAvailability ? (
        <p className="text-xs text-foreground/60 text-center">
          {registerAvailability.max_users <= 0
            ? t("auth.register.quotaUnlimited", {
                current: registerAvailability.current_users,
              })
            : t("auth.register.quota", {
                current: registerAvailability.current_users,
                max: registerAvailability.max_users,
              })}
        </p>
      ) : null}

      <Button
        type="button"
        className={AUTH_PRIMARY_BTN}
        onClick={goNext}
        disabled={
          Boolean(registerAvailability && (!canRegister || !registerAvailability.available))
        }
      >
        {TOTAL_STEPS > 1 ? (
          <>
            {t("auth.register.stepNext")}
            <ArrowRight className="ml-2 h-5 w-5" />
          </>
        ) : (
          <>
            <UserPlus className="mr-2 h-5 w-5" />
            {t("auth.register.submit")}
          </>
        )}
      </Button>
    </>
  );

  // ---- Step 1: Telegram binding ----

  const step1 = (
    <>
      <div className="space-y-2">
        <Label className="ml-1">
          {t("auth.register.telegramBinding", {
            suffix: forceBindTelegram ? " *" : t("common.optional"),
          })}
        </Label>
        <div className="rounded-xl border border-amber-200 bg-amber-50 px-4 py-3 text-sm text-amber-900">
          <p className="font-medium">{t("auth.register.openBotChat")}</p>
          <p className="mt-1 leading-relaxed">{t("auth.register.bindInstructions")}</p>
          {botUsername ? (
            <p className="mt-2 inline-flex items-center gap-1.5 text-xs text-amber-900">
              <Bot className="h-3.5 w-3.5" />
              <span>{t("auth.register.siteBot")}</span>
              <a
                href={botUrl}
                target="_blank"
                rel="noopener noreferrer"
                className="font-medium underline-offset-2 hover:underline"
              >
                @{botUsername}
              </a>
            </p>
          ) : (
            <p className="mt-2 text-xs text-amber-700">
              {t("auth.register.botNotConfigured")}
            </p>
          )}
        </div>
      </div>

      <div className="flex flex-col gap-3 sm:flex-row sm:items-center sm:flex-wrap">
        <Button
          type="button"
          className={AUTH_PRIMARY_BTN}
          onClick={handleCreateTelegramLink}
          disabled={isBindCodeLoading || (Boolean(telegramLink) && !bindConfirmed)}
        >
          {isBindCodeLoading ? (
            <Loader2 className="mr-2 h-4 w-4 animate-spin" />
          ) : (
            <ShieldPlus className="mr-2 h-4 w-4" />
          )}
          {t("auth.register.getBindCode")}
        </Button>
        {botUrl ? (
          <Button asChild type="button" variant="outline">
            <a href={botUrl} target="_blank" rel="noopener noreferrer">
              <Bot className="mr-2 h-4 w-4" />
              {t("auth.register.openBot", { username: botUsername })}
            </a>
          </Button>
        ) : null}
      </div>

      {telegramLink && !bindConfirmed ? (
        <div className="space-y-3 rounded-lg border border-border/70 bg-muted/50 px-3 py-3 text-sm">
          {telegramLink.deep_link ? (
            <Button asChild type="button" className={AUTH_PRIMARY_BTN}>
              <a href={telegramLink.deep_link} target="_blank" rel="noopener noreferrer">
                <Send className="mr-2 h-4 w-4" />
                {t("auth.register.openTelegramLink", { username: telegramLink.bot_username })}
              </a>
            </Button>
          ) : null}
          <p className="text-xs text-muted-foreground">{t("auth.register.manualCommandHint")}</p>
          <div className="flex flex-wrap items-center gap-2">
            <code className="min-w-0 max-w-full select-all break-all rounded bg-background px-2 py-1 font-mono text-sm">
              {telegramLink.manual_command}
            </code>
            <Button
              type="button"
              size="sm"
              variant="outline"
              onClick={() => {
                navigator.clipboard.writeText(telegramLink.manual_command).then(
                  () => toast({ title: t("common.copiedToClipboard"), variant: "success" }),
                  () => toast({ title: t("common.copyFailed"), variant: "destructive" }),
                );
              }}
            >
              {t("auth.register.copyCommand")}
            </Button>
            {botUrl && !telegramLink.deep_link ? (
              <Button asChild type="button" size="sm" variant="outline">
                <a href={botUrl} target="_blank" rel="noopener noreferrer">
                  <Bot className="mr-2 h-4 w-4" />
                  {t("auth.register.openBot", { username: botUsername })}
                </a>
              </Button>
            ) : null}
          </div>
          <p className="flex items-center gap-1 text-xs">
            <Loader2 className="h-3 w-3 animate-spin" />
            {t("auth.register.waitingVerification", {
              minutes: Math.max(0, Math.floor((telegramLink.expires_in ?? 0) / 60)),
            })}
          </p>
        </div>
      ) : null}

      {telegramLink && bindConfirmed ? (
        <div className="rounded-lg border border-emerald-300/60 bg-emerald-50 px-3 py-2 text-sm dark:border-emerald-700/60 dark:bg-emerald-900/30">
          <p className="font-semibold text-emerald-700 dark:text-emerald-300">
            {t("auth.register.telegramBound")}
          </p>
          <p className="text-xs text-emerald-700/80 dark:text-emerald-300/80">
            {t("auth.register.telegramBoundDescription")}
          </p>
        </div>
      ) : null}

      {/* Final action row */}
      <div className="flex items-center gap-3 pt-2">
        <Button
          type="button"
          variant="outline"
          size="sm"
          onClick={goBack}
          className="shrink-0"
        >
          <ArrowLeft className="mr-1.5 h-4 w-4" />
          {t("auth.register.stepBack")}
        </Button>
        {!forceBindTelegram && (
          <Button
            type="button"
            variant="ghost"
            size="sm"
            className="shrink-0"
            onClick={skipTelegramAndSubmit}
            disabled={isRegisterLoading}
          >
            {t("auth.register.stepSkip")}
          </Button>
        )}
        <Button
          type="button"
          className={AUTH_PRIMARY_BTN}
          onClick={handleFinalSubmit}
          disabled={
            isRegisterLoading ||
            (forceBindTelegram && !bindConfirmed) ||
            Boolean(registerAvailability && (!canRegister || !registerAvailability.available))
          }
        >
          {isRegisterLoading ? (
            <Loader2 className="mr-2 h-5 w-5 animate-spin" />
          ) : (
            <UserPlus className="mr-2 h-5 w-5" />
          )}
          {t("auth.register.submit")}
        </Button>
      </div>
    </>
  );

  // ---- Render ----

  return (
    <>
      {TOTAL_STEPS > 1 && <AuthStepDots total={TOTAL_STEPS} current={step} />}
      <AuthBrand
        subtitle={registerRequiresCode ? t("auth.register.introWithCode") : undefined}
      />

      {telegramLinks.length > 0 && step === 0 && (
        <div className="rounded-xl border border-border/70 bg-muted/40 px-4 py-3 text-sm">
          <div className="mb-2 flex items-center gap-2 font-semibold text-foreground">
            <Send className="h-4 w-4 text-muted-foreground" />
            {t("auth.register.telegramCommunity")}
          </div>
          <div className="flex flex-wrap gap-2">
            {telegramLinks.map((item) => (
              <a
                key={item.url}
                href={item.url}
                target="_blank"
                rel="noopener noreferrer"
                className="rounded-md border border-border/70 bg-background px-2.5 py-1 text-xs font-medium text-foreground hover:bg-muted"
              >
                {item.label}
              </a>
            ))}
          </div>
        </div>
      )}

      <div className="space-y-4">{step === 0 ? step0 : step1}</div>

      <div className="pt-1 text-center">
        <Link href="/login" className="text-sm font-medium text-foreground/80 underline-offset-4 hover:text-foreground hover:underline">
          {t("auth.register.backToLogin")}
        </Link>
      </div>
    </>
  );
}
