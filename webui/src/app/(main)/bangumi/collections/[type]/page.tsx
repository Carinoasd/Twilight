"use client";

import { useCallback, useEffect, useMemo, useState } from "react";
import { useParams } from "next/navigation";
import Link from "next/link";
import { Loader2, RefreshCw } from "lucide-react";
import { api } from "@/lib/api";
import { API_BASE } from "@/lib/api-request";
import { useI18n } from "@/lib/i18n";
import { useAsyncResource } from "@/hooks/use-async-resource";
import { useToast } from "@/hooks/use-toast";
import { BangumiFeatureGate } from "@/components/bangumi-feature-gate";
import { Button } from "@/components/ui/button";
import { Card, CardContent } from "@/components/ui/card";
import { Badge } from "@/components/ui/badge";
import { Input } from "@/components/ui/input";
import { Label } from "@/components/ui/label";
import { Textarea } from "@/components/ui/textarea";
import { Switch } from "@/components/ui/switch";
import {
  Dialog,
  DialogContent,
  DialogHeader,
  DialogTitle,
  DialogDescription,
  DialogFooter,
} from "@/components/ui/dialog";

const kinds = [
  "wishlist",
  "collected",
  "watching",
  "on_hold",
  "dropped",
] as const;
const selectClass =
  "h-9 max-w-full rounded-md border bg-background px-2 text-sm";
export default function BangumiCollectionPage() {
  const params = useParams<{ type: string }>();
  const type = Number(params.type);
  return (
    <BangumiFeatureGate manageOnly>
      <Collections
        key={params.type}
        type={Number.isInteger(type) && type >= 1 && type <= 5 ? type : 3}
      />
    </BangumiFeatureGate>
  );
}
function Collections({ type }: { type: number }) {
  const { t } = useI18n(),
    { toast } = useToast();
  const [page, setPage] = useState(1),
    [size, setSize] = useState(24),
    [refresh, setRefresh] = useState(0);
  const [view, setView] = useState("grid"),
    [sort, setSort] = useState("default"),
    [query, setQuery] = useState("");
  const [editing, setEditing] = useState<any | null>(null),
    [saving, setSaving] = useState(false);
  const [editType, setEditType] = useState(type),
    [episode, setEpisode] = useState("0"),
    [rate, setRate] = useState(0);
  const [comment, setComment] = useState(""),
    [tags, setTags] = useState(""),
    [privateCollection, setPrivate] = useState(false);
  const load = useCallback(
    async (signal?: AbortSignal) => {
      const res = await api.getBangumiCollections(
        type,
        size,
        (page - 1) * size,
        refresh > 0,
        signal,
      );
      if (!res.success || !res.data) throw new Error(res.message);
      return res.data;
    },
    [type, size, page, refresh],
  );
  const {
    data,
    isLoading,
    error,
    execute: reload,
  } = useAsyncResource(load, { throwOnError: false });
  useEffect(() => {
    if (!data || isLoading || error) return;
    const last = Math.max(1, Math.ceil(data.total / size));
    if (page > last) setPage(last);
  }, [data, isLoading, error, page, size]);
  const items = useMemo(() => {
    const term = query.trim().toLocaleLowerCase();
    const list = [...(data?.entries || [])].filter(
      (item) =>
        !term ||
        [item.subject?.name, item.subject?.name_cn, ...(item.tags || [])].some(
          (value) =>
            String(value || "")
              .toLocaleLowerCase()
              .includes(term),
        ),
    );
    if (sort === "progress")
      list.sort((a, b) => (b.ep_status || 0) - (a.ep_status || 0));
    if (sort === "rating") list.sort((a, b) => (b.rate || 0) - (a.rate || 0));
    if (sort === "updated")
      list.sort((a, b) => (b.updated_at || 0) - (a.updated_at || 0));
    return list;
  }, [data, query, sort]);
  const title = (item: any) =>
    item?.subject?.name_cn || item?.subject?.name || `#${item?.subject_id}`;
  const edit = (item: any) => {
    setEditing(item);
    setEditType(item.type || type);
    setEpisode(String(item.ep_status || 0));
    setRate(item.rate || 0);
    setComment(item.comment || "");
    setTags((item.tags || []).join(" "));
    setPrivate(Boolean(item.private));
  };
  const save = async () => {
    if (!editing || saving) return;
    const progress = Number(episode),
      tagList = tags.trim() ? tags.trim().split(/\s+/) : [];
    if (
      !Number.isInteger(progress) ||
      progress < 0 ||
      progress > 5000 ||
      tagList.length > 10 ||
      tagList.some((tag) => Array.from(tag).length > 30) ||
      Array.from(comment).length > 1000
    ) {
      toast({
        title: t("bangumiWork.invalidCollection"),
        variant: "destructive",
      });
      return;
    }
    setSaving(true);
    try {
      const res = await api.updateBangumiCollection(
        String(editing.subject_id),
        {
          type: editType,
          rate,
          comment,
          tags: tagList,
          private: privateCollection,
          ...(editType === 3 ? { ep_status: progress } : {}),
        },
      );
      if (!res.success) throw new Error(res.message);
      setEditing(null);
      toast({ title: t("bangumiWork.saved") });
      await reload();
    } catch {
      toast({
        title: t("bangumiWork.collectionSaveFailed"),
        variant: "destructive",
      });
    } finally {
      setSaving(false);
    }
  };
  return (
    <div className="min-w-0 space-y-5">
      <header className="flex flex-wrap items-start justify-between gap-3">
        <div>
          <Link href="/bangumi" className="text-sm text-primary underline">
            {t("bangumiWork.back")}
          </Link>
          <h1 className="mt-2 text-2xl font-bold">
            {t(`bangumiWork.${kinds[type - 1]}`)}
          </h1>
          <p className="mt-1 text-sm text-muted-foreground">
            {t("bangumiWork.collectionTotal", { count: data?.total || 0 })}
          </p>
        </div>
        <Button
          variant="outline"
          disabled={isLoading}
          onClick={() => setRefresh((v) => v + 1)}
        >
          <RefreshCw className="mr-2 h-4 w-4" />
          {t("bangumiWork.refresh")}
        </Button>
      </header>
      <nav
        className="flex flex-wrap gap-2"
        aria-label={t("bangumiWork.manage")}
      >
        {kinds.map((kind, i) => (
          <Button
            key={kind}
            asChild
            size="sm"
            variant={type === i + 1 ? "default" : "outline"}
          >
            <Link href={`/bangumi/collections/${i + 1}`} prefetch={false}>
              {t(`bangumiWork.${kind}`)}
            </Link>
          </Button>
        ))}
      </nav>
      <div className="flex flex-wrap gap-3">
        <Input
          className="w-full sm:w-60"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          placeholder={t("bangumiWork.searchPage")}
          aria-label={t("bangumiWork.searchPage")}
        />
        <select
          className={selectClass}
          value={sort}
          onChange={(e) => setSort(e.target.value)}
          aria-label={t("bangumiWork.sort")}
        >
          <option value="default">{t("bangumiWork.defaultSort")}</option>
          <option value="updated">{t("bangumiWork.updated")}</option>
          <option value="progress">{t("bangumiWork.progress")}</option>
          <option value="rating">{t("bangumiWork.rating")}</option>
        </select>
        <select
          className={selectClass}
          value={view}
          onChange={(e) => setView(e.target.value)}
          aria-label={t("bangumiWork.view")}
        >
          <option value="grid">{t("bangumiWork.grid")}</option>
          <option value="list">{t("bangumiWork.list")}</option>
        </select>
        <select
          className={selectClass}
          value={size}
          onChange={(e) => {
            setSize(Number(e.target.value));
            setPage(1);
          }}
          aria-label={t("bangumiWork.pageSize")}
        >
          {[12, 24, 48, 96].map((n) => (
            <option key={n} value={n}>
              {n}
            </option>
          ))}
        </select>
      </div>
      {data?.cached && (
        <p className="text-xs text-muted-foreground">
          {t("bangumiWork.cached")}
          {data.cache_updated_at
            ? ` · ${new Date(data.cache_updated_at * 1000).toLocaleString()}`
            : ""}
        </p>
      )}
      {error ? (
        <p role="alert" className="text-destructive">
          {t("bangumiWork.loadFailed")}
        </p>
      ) : isLoading ? (
        <Loader2 className="h-6 w-6 animate-spin" />
      ) : (
        <div
          className={
            view === "grid"
              ? "grid grid-cols-1 gap-4 lg:grid-cols-2 xl:grid-cols-3"
              : "space-y-3"
          }
        >
          {items.length === 0 && (
            <p className="py-8 text-muted-foreground">
              {t("bangumiWork.empty")}
            </p>
          )}
          {items.map((item) => (
            <Card key={item.subject_id}>
              <CardContent className="grid grid-cols-[72px_minmax(0,1fr)] gap-3 p-4 sm:grid-cols-[96px_minmax(0,1fr)]">
                {/* eslint-disable-next-line @next/next/no-img-element -- authenticated service cover adapter */}
                <img
                  src={`${API_BASE}/api/v2/bangumi/covers/${encodeURIComponent(item.subject_id)}`}
                  alt=""
                  className="aspect-[2/3] w-full rounded-md bg-muted object-cover"
                  loading="lazy"
                />
                <div className="min-w-0 space-y-2">
                  <h2 className="line-clamp-2 break-words font-semibold">
                    {title(item)}
                  </h2>
                  <p className="text-sm text-muted-foreground">
                    {t("bangumiWork.progress")}: {item.ep_status || 0}
                    {item.subject?.eps ? ` / ${item.subject.eps}` : ""} ·{" "}
                    {t("bangumiWork.rating")}: {item.rate || 0}
                  </p>
                  {item.private && (
                    <Badge variant="outline">{t("bangumiWork.private")}</Badge>
                  )}
                  {!!item.tags?.length && (
                    <div className="flex flex-wrap gap-1">
                      {item.tags.map((tag: string) => (
                        <Badge
                          key={tag}
                          variant="secondary"
                          className="max-w-full break-all"
                        >
                          {tag}
                        </Badge>
                      ))}
                    </div>
                  )}
                  {item.comment && (
                    <p className="line-clamp-3 break-words text-sm text-muted-foreground">
                      {item.comment}
                    </p>
                  )}
                  <div className="flex flex-wrap items-center gap-3">
                    <Button
                      size="sm"
                      variant="outline"
                      disabled={saving}
                      onClick={() => edit(item)}
                    >
                      {t("bangumiWork.edit")}
                    </Button>
                    <a
                      href={`https://bgm.tv/subject/${encodeURIComponent(item.subject_id)}`}
                      target="_blank"
                      rel="noopener noreferrer"
                      className="text-sm text-primary underline"
                    >
                      Bangumi
                    </a>
                  </div>
                </div>
              </CardContent>
            </Card>
          ))}
        </div>
      )}
      <div className="flex flex-wrap items-center justify-center gap-3">
        <Button
          variant="outline"
          size="sm"
          disabled={page <= 1 || isLoading}
          onClick={() => setPage((p) => p - 1)}
        >
          {t("bangumiWork.previous")}
        </Button>
        <span className="text-sm">
          {page} / {Math.max(1, Math.ceil((data?.total || 0) / size))}
        </span>
        <Button
          variant="outline"
          size="sm"
          disabled={isLoading || page * size >= (data?.total || 0)}
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
        <DialogContent className="max-h-[90dvh] overflow-y-auto sm:max-w-lg">
          <DialogHeader>
            <DialogTitle>{t("bangumiWork.edit")}</DialogTitle>
            <DialogDescription>
              {editing ? title(editing) : ""}
            </DialogDescription>
          </DialogHeader>
          <fieldset disabled={saving} className="min-w-0 space-y-4">
            <div className="space-y-2">
              <Label htmlFor="collection-state">
                {t("bangumiWork.status")}
              </Label>
              <select
                id="collection-state"
                className={`${selectClass} w-full`}
                value={editType}
                onChange={(e) => setEditType(Number(e.target.value))}
              >
                {kinds.map((kind, i) => (
                  <option key={kind} value={i + 1}>
                    {t(`bangumiWork.${kind}`)}
                  </option>
                ))}
              </select>
            </div>
            {editType === 3 && (
              <div className="space-y-2">
                <Label htmlFor="collection-progress">
                  {t("bangumiWork.progressThrough")}
                </Label>
                <Input
                  id="collection-progress"
                  type="number"
                  min={0}
                  max={5000}
                  value={episode}
                  onChange={(e) => setEpisode(e.target.value)}
                />
                <p className="text-xs text-muted-foreground">
                  {t("bangumiWork.progressHint")}
                </p>
              </div>
            )}
            {editType === 2 && (
              <p className="rounded-md bg-muted p-3 text-sm">
                {t("bangumiWork.completedHint")}
              </p>
            )}
            <div className="space-y-2">
              <Label htmlFor="collection-rating">
                {t("bangumiWork.rating")}
              </Label>
              <select
                id="collection-rating"
                className={`${selectClass} w-full`}
                value={rate}
                onChange={(e) => setRate(Number(e.target.value))}
              >
                {Array.from({ length: 11 }, (_, n) => (
                  <option key={n} value={n}>
                    {n}
                  </option>
                ))}
              </select>
            </div>
            <div className="space-y-2">
              <Label htmlFor="collection-comment">
                {t("bangumiWork.comment")}
              </Label>
              <Textarea
                id="collection-comment"
                maxLength={1000}
                rows={4}
                value={comment}
                onChange={(e) => setComment(e.target.value)}
              />
            </div>
            <div className="space-y-2">
              <Label htmlFor="collection-tags">{t("bangumiWork.tags")}</Label>
              <Input
                id="collection-tags"
                value={tags}
                onChange={(e) => setTags(e.target.value)}
              />
              <p className="text-xs text-muted-foreground">
                {t("bangumiWork.tagsHint")}
              </p>
            </div>
            <div className="flex items-center justify-between gap-4">
              <Label htmlFor="collection-private">
                {t("bangumiWork.private")}
              </Label>
              <Switch
                id="collection-private"
                checked={privateCollection}
                onCheckedChange={setPrivate}
              />
            </div>
          </fieldset>
          <DialogFooter>
            <Button
              variant="outline"
              disabled={saving}
              onClick={() => setEditing(null)}
            >
              {t("bangumiWork.cancel")}
            </Button>
            <Button disabled={saving} onClick={() => void save()}>
              {saving && <Loader2 className="mr-2 h-4 w-4 animate-spin" />}
              {t("bangumiWork.save")}
            </Button>
          </DialogFooter>
        </DialogContent>
      </Dialog>
    </div>
  );
}
