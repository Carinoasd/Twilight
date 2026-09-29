"use client";

import { useCallback, useState } from "react";
import { Loader2, RefreshCw } from "lucide-react";
import { api, type BangumiWatchItem } from "@/lib/api";
import { useAsyncResource } from "@/hooks/use-async-resource";
import { useToast } from "@/hooks/use-toast";
import { useI18n } from "@/lib/i18n";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@/components/ui/dialog";

const states = [
  "pending",
  "needs_review",
  "failed",
  "success",
  "ignored",
] as const;
export function BangumiWatchRecords({ revision = 0 }: { revision?: number }) {
  const { t } = useI18n();
  const { toast } = useToast();
  const [page, setPage] = useState(1),
    [filter, setFilter] = useState("");
  const [editing, setEditing] = useState<BangumiWatchItem | null>(null);
  const [subject, setSubject] = useState(""),
    [episode, setEpisode] = useState("1"),
    [saving, setSaving] = useState(false);
  const load = useCallback(
    async (signal?: AbortSignal) => {
      void revision;
      const res = await api.getBangumiWatchRecords(page, filter, signal);
      if (!res.success || !res.data) throw new Error(res.message);
      return res.data;
    },
    [page, filter, revision],
  );
  const {
    data,
    isLoading,
    error,
    execute: reload,
  } = useAsyncResource(load, { throwOnError: false });
  const change = async (
    item: BangumiWatchItem,
    action: "confirm" | "ignore" | "retry",
  ) => {
    if (saving) return;
    if (
      action === "confirm" &&
      (!/^\d+$/.test(subject) ||
        Number(subject) <= 0 ||
        !Number.isInteger(Number(episode)) ||
        Number(episode) < 0)
    ) {
      toast({ title: t("bangumiWork.invalidMapping"), variant: "destructive" });
      return;
    }
    setSaving(true);
    try {
      const res = await api.updateBangumiWatch(item.key, {
        action,
        ...(action === "confirm"
          ? { subject_id: subject, episode: Number(episode) }
          : {}),
      });
      if (!res.success) throw new Error(res.message);
      setEditing(null);
      await reload();
      toast({ title: t("bangumiWork.saved") });
    } catch {
      toast({ title: t("bangumiWork.saveFailed"), variant: "destructive" });
    } finally {
      setSaving(false);
    }
  };
  return (
    <section
      className="min-w-0 space-y-4"
      aria-label={t("bangumiWork.records")}
    >
      <p className="text-sm text-muted-foreground">
        {t("bangumiWork.recordsHint")}
      </p>
      <div className="flex flex-wrap items-center gap-3">
        <Label htmlFor="watch-filter">{t("bangumiWork.status")}</Label>
        <select
          id="watch-filter"
          className="h-9 rounded-md border bg-background px-3 text-sm"
          value={filter}
          onChange={(e) => {
            setFilter(e.target.value);
            setPage(1);
          }}
        >
          <option value="">{t("bangumiWork.all")}</option>
          {states.map((s) => (
            <option key={s} value={s}>
              {t(`bangumiWork.${s}`)}
            </option>
          ))}
        </select>
        <Button
          variant="outline"
          size="sm"
          disabled={isLoading}
          onClick={() => void reload()}
        >
          <RefreshCw className="mr-2 h-4 w-4" />
          {t("bangumiWork.refresh")}
        </Button>
      </div>
      {error ? (
        <p role="alert" className="text-sm text-destructive">
          {t("bangumiWork.loadFailed")}
        </p>
      ) : isLoading ? (
        <Loader2 className="h-5 w-5 animate-spin" />
      ) : (
        <div className="max-h-[65dvh] space-y-3 overflow-y-auto overscroll-contain pr-1">
          {data?.items.length === 0 && (
            <p className="py-8 text-center text-muted-foreground">
              {t("bangumiWork.empty")}
            </p>
          )}
          {data?.items.map((item) => (
            <Card key={item.key}>
              <CardContent className="space-y-3 p-4">
                <div className="flex flex-wrap items-start justify-between gap-2">
                  <div className="min-w-0 flex-1">
                    <h3 className="break-words font-medium">
                      {item.series_name || item.title}
                    </h3>
                    <p className="break-words text-sm text-muted-foreground">
                      {item.title}
                      {item.index_number > 0 ? ` · #${item.index_number}` : ""}
                    </p>
                  </div>
                  <Badge variant="outline">
                    {t(`bangumiWork.${item.status}`)}
                  </Badge>
                </div>
                <p className="text-xs text-muted-foreground">
                  {new Date(item.played_at * 1000).toLocaleString()}
                </p>
                {item.subject_id && (
                  <a
                    className="block break-words text-sm text-primary underline"
                    target="_blank"
                    rel="noopener noreferrer"
                    href={`https://bgm.tv/subject/${encodeURIComponent(item.subject_id)}`}
                  >
                    {item.subject_name || `#${item.subject_id}`}
                  </a>
                )}
                {item.message && (
                  <p className="break-words text-sm text-muted-foreground">
                    {item.message}
                  </p>
                )}
                <div className="flex flex-wrap gap-2">
                  <Button
                    size="sm"
                    variant="outline"
                    disabled={saving}
                    onClick={() => {
                      setEditing(item);
                      setSubject(item.subject_id || "");
                      setEpisode(
                        String(item.episode || item.index_number || 0),
                      );
                    }}
                  >
                    {t("bangumiWork.confirmMapping")}
                  </Button>
                  {item.status !== "success" && (
                    <Button
                      size="sm"
                      variant="ghost"
                      disabled={saving}
                      onClick={() =>
                        void change(
                          item,
                          item.status === "ignored" ? "retry" : "ignore",
                        )
                      }
                    >
                      {t(
                        item.status === "ignored"
                          ? "bangumiWork.retry"
                          : "bangumiWork.ignore",
                      )}
                    </Button>
                  )}
                  {item.status === "failed" && (
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={saving}
                      onClick={() => void change(item, "retry")}
                    >
                      {t("bangumiWork.retry")}
                    </Button>
                  )}
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}
      <div className="flex flex-wrap items-center justify-center gap-3">
        <Button
          size="sm"
          variant="outline"
          disabled={page <= 1 || isLoading}
          onClick={() => setPage((p) => p - 1)}
        >
          {t("bangumiWork.previous")}
        </Button>
        <span className="text-sm">
          {page} / {Math.max(1, Math.ceil((data?.total || 0) / 20))}
        </span>
        <Button
          size="sm"
          variant="outline"
          disabled={isLoading || page * 20 >= (data?.total || 0)}
          onClick={() => setPage((p) => p + 1)}
        >
          {t("bangumiWork.next")}
        </Button>
      </div>
      <Dialog
        open={!!editing}
        onOpenChange={(open) => {
          if (!open && !saving) setEditing(null);
        }}
      >
        <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-md">
          <DialogHeader>
            <DialogTitle>{t("bangumiWork.confirmMapping")}</DialogTitle>
            <DialogDescription>
              {t("bangumiWork.confirmHint")}
            </DialogDescription>
          </DialogHeader>
          <div className="space-y-2">
            <Label htmlFor="watch-subject">{t("bangumiWork.subjectID")}</Label>
            <Input
              id="watch-subject"
              inputMode="numeric"
              value={subject}
              onChange={(e) => setSubject(e.target.value)}
              disabled={saving}
            />
          </div>
          <div className="space-y-2">
            <Label htmlFor="watch-episode">{t("bangumiWork.episode")}</Label>
            <Input
              id="watch-episode"
              type="number"
              min={0}
              max={10000}
              value={episode}
              onChange={(e) => setEpisode(e.target.value)}
              disabled={saving}
            />
          </div>
          <DialogFooter>
            <Button
              variant="outline"
              disabled={saving}
              onClick={() => setEditing(null)}
            >
              {t("bangumiWork.cancel")}
            </Button>
            <Button
              disabled={saving}
              onClick={() => editing && void change(editing, "confirm")}
            >
              {saving && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
              {t("bangumiWork.confirmWatched")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </section>
  );
}
