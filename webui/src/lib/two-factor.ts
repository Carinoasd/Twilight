export interface TwoFactorChallenge { request: string; expires_at: number }
export interface TwoFactorStatus { enabled: boolean; enabled_at: number; recovery_remaining: number; enrollment_allowed: boolean; key_ready: boolean }
export interface TwoFactorSetup extends TwoFactorChallenge { secret: string; uri: string }

export function readTwoFactorChallenge(value: unknown): TwoFactorChallenge | null {
  if (!value || typeof value !== "object") return null;
  const data = value as Record<string, unknown>;
  if (data.two_factor_required !== true || typeof data.request !== "string" || !/^[a-f0-9]{64}$/.test(data.request)) return null;
  if (typeof data.expires_at !== "number" || !Number.isFinite(data.expires_at) || data.expires_at * 1000 <= Date.now() || data.expires_at * 1000 > Date.now() + 185000) return null;
  return { request: data.request, expires_at: data.expires_at };
}

export function twoFactorErrorKey(code?: string) {
  if (code === "AUTH_TWO_FACTOR_CODE_INVALID") return "twoFactor.invalidCode" as const;
  if (code === "AUTH_TWO_FACTOR_INVALID") return "twoFactor.expired" as const;
  if (code === "AUTH_TWO_FACTOR_UNAVAILABLE") return "twoFactor.unavailable" as const;
  if (code === "RATE_LIMITED" || code === "AUTH_LOGIN_RATE_LIMITED") return "twoFactor.rateLimited" as const;
  if (code === "AUTH_PASSWORD_OLD_MISMATCH") return "twoFactor.wrongPassword" as const;
  return "twoFactor.failed" as const;
}
