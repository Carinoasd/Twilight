"use client";

import { useCallback, useState } from "react";
import Link from "next/link";
import { BookOpen, Loader2, RefreshCw } from "lucide-react";
import { api } from "@/lib/api";
import { useI18n } from "@/lib/i18n";
import { useAsyncResource } from "@/hooks/use-async-resource";
import { useToast } from "@/hooks/use-toast";
import { useConfirm } from "@/components/ui/confirm-dialog";
import { useAuthStore } from "@/store/auth";
import { BangumiFeatureGate } from "@/components/bangumi-feature-gate";
import { BangumiWatchRecords } from "@/components/bangumi-watch-records";
import { Card, CardHeader, CardTitle, CardContent } from "@/components/ui/card";
import { Button } from "@/components/ui/button";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Switch } from "@/components/ui/switch";

const categories = [
  { key: "watching", type: 3 },
  { key: "collected", type: 2 },
  { key: "wishlist", type: 1 },
  { key: "on_hold", type: 4 },
  { key: "dropped", type: 5 },
] as const;
export default function BangumiPage() {
  return (
    <BangumiFeatureGate>
      <BangumiContent />
    </BangumiFeatureGate>
  );
}
function BangumiContent() {
  const { t } = useI18n(),
    { toast } = useToast(),
    { confirm } = useConfirm();
  const fetchUser = useAuthStore((s) => s.fetchUser);
  const [token, setToken] = useState(""),
    [syncMode, setSyncMode] = useState(false),
    [manageMode, setManageMode] = useState(false);
  const [saving, setSaving] = useState(false),
    [syncing, setSyncing] = useState(false),
    [tab, setTab] = useState("overview"),
    [revision, setRevision] = useState(0);
  const load = useCallback(async (signal?: AbortSignal) => {
    const res = await api.getBangumiSummary(signal);
    if (!res.success || !res.data) throw new Error(res.message);
    if (signal?.aborted) throw new DOMException("aborted", "AbortError");
    setSyncMode(res.data.status.bgm_mode);
    setManageMode(res.data.status.bgm_manage_mode);
    return res.data;
  }, []);
  const {
    data,
    isLoading,
    error,
    execute: reload,
  } = useAsyncResource(load, { throwOnError: false });
  const save = async (clear = false) => {
    if (saving) return;
    if (
      clear &&
      !(await confirm({
        title: t("bangumiWork.clearToken"),
        description: t("bangumiWork.clearTokenHint"),
        tone: "danger",
        confirmLabel: t("bangumiWork.clearToken"),
      }))
    )
      return;
    setSaving(true);
    try {
      const payload: Parameters<typeof api.updateMySettings>[0] = {};
      if (data?.status.sync_enabled)
        payload.bgm_mode = clear ? false : syncMode;
      if (data?.status.manage_enabled)
        payload.bgm_manage_mode = clear ? false : manageMode;
      if (clear || token.trim()) payload.bgm_token = clear ? "" : token.trim();
      const res = await api.updateMySettings(payload);
      if (!res.success) throw new Error(res.message);
      setToken("");
      await fetchUser();
      await reload();
      setRevision((r) => r + 1);
      toast({ title: t("bangumiWork.saved") });
    } catch {
      toast({ title: t("bangumiWork.saveFailed"), variant: "destructive" });
    } finally {
      setSaving(false);
    }
  };
  const sync = async () => {
    if (syncing) return;
    setSyncing(true);
    try {
      const res = await api.triggerBangumiSync();
      if (!res.success || !res.data) throw new Error(res.message);
      toast({
        title: t(
          res.data.failed > 0
            ? "bangumiWork.syncPartial"
            : "bangumiWork.syncDone",
        ),
        description: t("bangumiWork.syncCounts", {
          synced: res.data.synced,
          skipped: res.data.skipped,
          failed: res.data.failed,
        }),
        variant: res.data.failed > 0 ? "destructive" : "default",
      });
      await reload();
      setRevision((r) => r + 1);
    } catch {
      toast({ title: t("bangumiWork.syncFailed"), variant: "destructive" });
    } finally {
      setSyncing(false);
    }
  };
  const clearHistory = async () => {
    if (
      !(await confirm({
        title: t("bangumiWork.clearHistory"),
        description: t("bangumiWork.clearHistoryHint"),
        tone: "danger",
        confirmLabel: t("bangumiWork.clearHistory"),
      }))
    )
      return;
    try {
      const res = await api.clearBangumiSyncHistory();
      if (!res.success) throw new Error(res.message);
      await reload();
    } catch {
      toast({ title: t("bangumiWork.saveFailed"), variant: "destructive" });
    }
  };
  if (isLoading && !data) return <Loader2 className="h-6 w-6 animate-spin" />;
  return (
    <div className="min-w-0 space-y-6 page-enter">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <h1 className="flex items-center gap-2 text-2xl font-bold">
            <BookOpen />
            {t("bangumiWork.title")}
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            {t("bangumiWork.description")}
          </p>
        </div>
        <Button
          variant="outline"
          disabled={isLoading || saving || syncing}
          onClick={() => void reload()}
        >
          <RefreshCw className="mr-2 h-4 w-4" />
          {t("bangumiWork.refresh")}
        </Button>
      </header>
      {(error || data?.account_error || data?.account?.expired) && (
        <p
          role="alert"
          className="rounded-lg border border-destructive p-4 text-sm text-destructive"
        >
          {t(
            data?.account?.expired
              ? "bangumiWork.tokenExpired"
              : "bangumiWork.accountError",
          )}
        </p>
      )}
      {data?.collections_partial && (
        <p role="status" className="text-sm text-muted-foreground">
          {t("bangumiWork.partialCollections")}
        </p>
      )}
      <div className="flex flex-wrap gap-2">
        <Button
          variant={tab === "overview" ? "default" : "outline"}
          onClick={() => setTab("overview")}
        >
          {t("bangumiWork.overview")}
        </Button>
        {data?.status.sync_enabled && (
          <Button
            variant={tab === "records" ? "default" : "outline"}
            onClick={() => setTab("records")}
          >
            {t("bangumiWork.records")}
          </Button>
        )}
      </div>
      {data?.status.sync_enabled && (
        <Card>
          <CardHeader>
            <CardTitle className="text-base">{t("bangumiWork.sync")}</CardTitle>
          </CardHeader>
          <CardContent className="space-y-4">
            <div className="grid grid-cols-2 gap-3">
              <div className="rounded-lg bg-muted p-3">
                <p className="text-2xl font-semibold">
                  {data.status.total_records}
                </p>
                <p className="text-sm text-muted-foreground">
                  {t("bangumiWork.totalRecords")}
                </p>
              </div>
              <div className="rounded-lg bg-muted p-3">
                <p className="text-2xl font-semibold">
                  {data.status.synced_count}
                </p>
                <p className="text-sm text-muted-foreground">
                  {t("bangumiWork.syncedItems")}
                </p>
              </div>
            </div>
            <p className="text-sm text-muted-foreground">
              {t("bangumiWork.syncHint")}
            </p>
            <Button
              disabled={syncing || saving || !data.status.sync_ready}
              onClick={() => void sync()}
            >
              {syncing && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
              {t("bangumiWork.syncNow")}
            </Button>
          </CardContent>
        </Card>
      )}
      {tab === "records" ? (
        <BangumiWatchRecords revision={revision} />
      ) : (
        <>
          {data?.account && !data.account.expired && (
            <Card>
              <CardContent className="space-y-2 p-5">
                <h2 className="break-words font-semibold">
                  {data.account.nickname || data.account.username}
                </h2>
                <p className="break-words text-sm text-muted-foreground">
                  {data.account.sign}
                </p>
                <a
                  className="text-sm text-primary underline"
                  href={`https://bgm.tv/user/${encodeURIComponent(data.account.username || String(data.account.id))}`}
                  target="_blank"
                  rel="noopener noreferrer"
                >
                  {t("bangumiWork.openProfile")}
                </a>
              </CardContent>
            </Card>
          )}
          {data?.status.manage_enabled && data.status.bgm_manage_mode && (
            <div className="grid grid-cols-2 gap-3 sm:grid-cols-5">
              {categories.map((cat) => (
                <Link
                  key={cat.key}
                  href={`/bangumi/collections/${cat.type}`}
                  prefetch={false}
                >
                  <Card className="h-full hover:bg-muted">
                    <CardContent className="p-4">
                      <p className="break-words text-sm">
                        {t(`bangumiWork.${cat.key}`)}
                      </p>
                      <p className="mt-2 text-2xl font-semibold">
                        {data.collections?.[cat.key]?.total ?? "—"}
                      </p>
                    </CardContent>
                  </Card>
                </Link>
              ))}
            </div>
          )}
          <Card>
            <CardHeader>
              <CardTitle className="text-base">
                {t("bangumiWork.settings")}
              </CardTitle>
            </CardHeader>
            <CardContent className="space-y-5">
              {data?.status.sync_enabled && (
                <div className="flex items-start justify-between gap-4">
                  <div>
                    <Label htmlFor="bgm-sync">
                      {t("bangumiWork.autoSync")}
                    </Label>
                    <p className="mt-1 text-sm text-muted-foreground">
                      {t("bangumiWork.syncHint")}
                    </p>
                  </div>
                  <Switch
                    id="bgm-sync"
                    checked={syncMode}
                    disabled={saving || syncing}
                    onCheckedChange={setSyncMode}
                  />
                </div>
              )}
              {data?.status.manage_enabled && (
                <div className="flex items-center justify-between gap-4">
                  <Label htmlFor="bgm-manage">{t("bangumiWork.manage")}</Label>
                  <Switch
                    id="bgm-manage"
                    checked={manageMode}
                    disabled={saving}
                    onCheckedChange={setManageMode}
                  />
                </div>
              )}
              <div className="space-y-2">
                <Label htmlFor="bgm-token">Access Token</Label>
                <Input
                  id="bgm-token"
                  type="password"
                  autoComplete="new-password"
                  value={token}
                  onChange={(e) => setToken(e.target.value)}
                  disabled={saving}
                  placeholder={t(
                    data?.status.token_set
                      ? "bangumiWork.tokenConfigured"
                      : "bangumiWork.tokenPlaceholder",
                  )}
                />
                <a
                  href="https://next.bgm.tv/demo/access-token"
                  target="_blank"
                  rel="noopener noreferrer"
                  className="text-sm text-primary underline"
                >
                  {t("bangumiWork.getToken")}
                </a>
              </div>
              <div className="flex flex-wrap gap-2">
                <Button
                  disabled={saving || syncing || !data}
                  onClick={() => void save()}
                >
                  {saving && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
                  {t("bangumiWork.save")}
                </Button>
                {data?.status.token_set && (
                  <Button
                    disabled={saving || syncing}
                    variant="outline"
                    onClick={() => void save(true)}
                  >
                    {t("bangumiWork.clearToken")}
                  </Button>
                )}
              </div>
            </CardContent>
          </Card>
          {!!data?.status.recent_logs?.length && (
            <Card>
              <CardHeader className="flex-row flex-wrap items-center justify-between gap-2">
                <CardTitle className="text-base">
                  {t("bangumiWork.history")}
                </CardTitle>
                <Button
                  size="sm"
                  variant="ghost"
                  disabled={syncing}
                  onClick={() => void clearHistory()}
                >
                  {t("bangumiWork.clearHistory")}
                </Button>
              </CardHeader>
              <CardContent className="max-h-[45dvh] space-y-3 overflow-y-auto">
                {data.status.recent_logs.map((log) => (
                  <div
                    key={log.id}
                    className="space-y-1 rounded-md bg-muted p-3 text-sm"
                  >
                    <p className="break-words font-medium">
                      {log.subject_name || log.record_item_id}
                    </p>
                    <p className="break-words text-muted-foreground">
                      {log.message}
                    </p>
                    <time className="text-xs text-muted-foreground">
                      {new Date(log.created_at * 1000).toLocaleString()}
                    </time>
                  </div>
                ))}
              </CardContent>
            </Card>
          )}
        </>
      )}
    </div>
  );
}
