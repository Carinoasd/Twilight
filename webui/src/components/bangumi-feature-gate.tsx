"use client";

import type { ReactNode } from "react";
import { Loader2 } from "lucide-react";
import { useSystemStore } from "@/store/system";
import { useI18n } from "@/lib/i18n";
import { FeatureDisabledNotice } from "@/components/feature-disabled";
import { Button } from "@/components/ui/button";

export function BangumiFeatureGate({
  children,
  manageOnly = false,
}: {
  children: ReactNode;
  manageOnly?: boolean;
}) {
  const { t } = useI18n();
  const info = useSystemStore((s) => s.info);
  const error = useSystemStore((s) => s.lastError);
  const fetchInfo = useSystemStore((s) => s.fetchInfo);
  if (!info)
    return error ? (
      <div className="space-y-3">
        <p role="alert">{t("bangumiWork.loadFailed")}</p>
        <Button onClick={() => void fetchInfo(true)}>
          {t("bangumiWork.refresh")}
        </Button>
      </div>
    ) : (
      <Loader2 className="h-6 w-6 animate-spin" />
    );
  const enabled = manageOnly
    ? info.features?.bangumi_manage !== false
    : info.features?.bangumi_manage !== false ||
      info.features?.bangumi_sync !== false;
  return enabled ? children : <FeatureDisabledNotice />;
}
