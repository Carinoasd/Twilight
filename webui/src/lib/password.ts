// Frontend password strength rule. Keep this in sync with the Go backend
// password validation policy.
// 提示文案走 i18n（translate 可在 React 组件树外使用），不再写死简体中文。
import { translate } from "@/lib/i18n";

export interface PasswordStrengthResult {
  ok: boolean;
  message: string;
  /** 0-4 rough strength score for visual indicator */
  score: number;
}

export function validatePasswordStrength(
  password: string,
  label = translate("passwordStrength.defaultLabel")
): PasswordStrengthResult {
  if (!password) {
    return { ok: false, message: translate("passwordStrength.required", { label }), score: 0 };
  }
  if (password.length > 128) {
    return { ok: false, message: translate("passwordStrength.tooLong", { label }), score: 0 };
  }

  const hasLower = /[a-z]/.test(password);
  const hasUpper = /[A-Z]/.test(password);
  const hasDigit = /[0-9]/.test(password);
  const hasSymbol = /[^A-Za-z0-9]/.test(password);
  const longEnough = password.length >= 8;

  const score =
    (longEnough ? 1 : 0) +
    (hasLower ? 1 : 0) +
    (hasUpper ? 1 : 0) +
    (hasDigit ? 1 : 0) +
    (hasSymbol ? 1 : 0);

  if (!longEnough) {
    return {
      ok: false,
      message: translate("passwordStrength.tooShort", { label }),
      score,
    };
  }
  if (!hasLower) {
    return { ok: false, message: translate("passwordStrength.needLower", { label }), score };
  }
  if (!hasUpper) {
    return { ok: false, message: translate("passwordStrength.needUpper", { label }), score };
  }
  if (!hasDigit) {
    return { ok: false, message: translate("passwordStrength.needDigit", { label }), score };
  }

  return { ok: true, message: translate("passwordStrength.ok"), score };
}

export function passwordStrengthLabel(score: number): {
  label: string;
  className: string;
} {
  if (score <= 1) return { label: translate("passwordStrength.levelWeak"), className: "text-destructive" };
  if (score === 2) return { label: translate("passwordStrength.levelFair"), className: "text-amber-500" };
  if (score === 3) return { label: translate("passwordStrength.levelGood"), className: "text-emerald-500" };
  return { label: translate("passwordStrength.levelStrong"), className: "text-emerald-600" };
}
