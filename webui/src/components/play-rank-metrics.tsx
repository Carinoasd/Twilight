"use client";

import { useI18n } from "@/lib/i18n";
import type { PlayRankSortBy } from "@/lib/api";

export function PlayRankMetrics({ plays, duration, sortBy, count, countLabel }: {
  plays: number;
  duration: string;
  sortBy: PlayRankSortBy;
  count?: number;
  countLabel?: string;
}) {
  const { t } = useI18n();
  const metricClass = (active: boolean) => active ? "text-sm font-medium text-foreground" : "text-xs text-muted-foreground";
  return (
    <div className="col-start-2 flex min-w-0 flex-wrap items-baseline gap-x-3 gap-y-1 sm:contents">
      {count !== undefined && (
        <span className="shrink-0 text-xs tabular-nums text-muted-foreground sm:w-14 sm:text-right">
          <span className="mr-1 sm:hidden">{countLabel}</span>{count}
        </span>
      )}
      <span className={`shrink-0 tabular-nums sm:w-12 sm:text-right ${metricClass(sortBy === "plays")}`}>
        <span className="mr-1 text-xs sm:hidden">{t("playRank.plays")}</span>{plays}
      </span>
      <span className={`shrink-0 tabular-nums sm:w-20 sm:text-right ${metricClass(sortBy === "duration")}`}>
        <span className="mr-1 text-xs sm:hidden">{t("playRank.duration")}</span>{duration}
      </span>
    </div>
  );
}
