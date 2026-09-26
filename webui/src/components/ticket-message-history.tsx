"use client";

import { useCallback, useEffect, useState } from "react";
import { Button } from "@/components/ui/button";
import { useAsyncResource } from "@/hooks/use-async-resource";
import { api, type Ticket, type TicketReply } from "@/lib/api";
import { useI18n } from "@/lib/i18n";

export interface TicketHistoryPage {
  items: TicketReply[];
  has_more: boolean;
  next_before: number;
  total: number;
}

// Keep one bounded message page mounted. Large historical tickets remain fully
// navigable without turning the dialog into an ever-growing DOM transcript.
export function TicketMessageHistory({ ticket, admin = false, onPage }: {
  ticket: Ticket;
  admin?: boolean;
  onPage: (page: TicketHistoryPage) => void;
}) {
  const { t } = useI18n();
  const [cursor, setCursor] = useState<number | null>(null);
  const loader = useCallback(async (signal?: AbortSignal) => {
    const response = await api.getTicketMessages(ticket.id, cursor ?? 0, admin, signal);
    if (!response.success || !response.data) throw new Error(response.message || t("common.networkError"));
    return response.data;
  }, [ticket.id, cursor, admin, t]);
  const { data, isLoading, error, execute } = useAsyncResource(loader, { immediate: cursor !== null, throwOnError: false });

  useEffect(() => { if (data) onPage(data); }, [data, onPage]);
  const replies = ticket.replies || [];
  const total = ticket.reply_count ?? replies.length;
  const first = replies[0]?.id ?? 0;
  const last = replies[replies.length - 1]?.id ?? 0;
  const select = (before: number) => {
    if (cursor === before) void execute();
    else setCursor(before);
  };
  if (!ticket.message_page) return null;
  return (
    <div className="space-y-2">
      <div className="flex flex-wrap items-center justify-between gap-2 text-xs text-muted-foreground">
        <span>{t("tickets.messageRange", { first, last, total })}</span>
        <div className="flex flex-wrap gap-2">
          {ticket.message_page.has_more && <Button size="sm" variant="outline" disabled={isLoading} onClick={() => select(ticket.message_page!.next_before)}>{t("tickets.olderMessages")}</Button>}
          {last < total && <Button size="sm" variant="outline" disabled={isLoading} onClick={() => select(0)}>{t("tickets.latestMessages")}</Button>}
        </div>
      </div>
      {error && <p role="alert" className="text-xs text-destructive">{error}<Button size="sm" variant="ghost" onClick={() => void execute()}>{t("common.retry")}</Button></p>}
    </div>
  );
}
