import type { Ticket, TicketAttachment, TicketReply, UserTicketListItem } from "./api-types";
import type {
  V2AdminTicketDetailResponse,
  V2AdminTicketListResponse,
  V2TicketAttachment,
  V2TicketReply,
  V2UserTicketDetailResponse,
  V2UserTicketListResponse,
} from "./api-types-v2";

/**
 * 工单的 V2 响应 → 前端模型的唯一转换层。
 *
 * 之前这些转换散落在 api.ts 的七八个方法里各写一遍，于是同一份数据在不同入口
 * 会变成不同形状：详情接口走一次映射、回复接口直接把原始对象丢给页面，页面里
 * 又各自用 `role === 0` 这种和后端枚举耦合的魔法数字判断身份。任何一处漏改，
 * 页面上「这条回复是谁发的」就会判反。现在所有入口都从这里过。
 */

const TICKET_STATUSES = ["open", "in_progress", "resolved", "closed"] as const;
const TICKET_PRIORITIES = ["low", "medium", "high", "urgent"] as const;

export type TicketStatus = (typeof TICKET_STATUSES)[number];
export type TicketPriority = (typeof TICKET_PRIORITIES)[number];

/** 后端角色枚举：管理员为 0。仅用于兼容旧数据，业务判断请用 is_admin。 */
const BACKEND_ROLE_ADMIN = 0;

function pickStatus(raw: unknown): TicketStatus {
  const value = String(raw ?? "").trim();
  return (TICKET_STATUSES as readonly string[]).includes(value) ? (value as TicketStatus) : "open";
}

function pickPriority(raw: unknown): TicketPriority {
  const value = String(raw ?? "").trim();
  return (TICKET_PRIORITIES as readonly string[]).includes(value) ? (value as TicketPriority) : "medium";
}

/** 0 在工单时间戳里表示「尚未发生」，统一折算成 undefined 便于页面判空。 */
function pickTimestamp(raw: unknown): number | undefined {
  const value = Number(raw);
  return Number.isFinite(value) && value > 0 ? value : undefined;
}

/**
 * 判断一条回复是不是管理员发的。
 *
 * is_admin 是后端下发的权威字段。author / role 只是为了兼容还没刷新的缓存与
 * 旧版本后端保留的兜底路径——不要把 role 当数字比较，它的取值是服务端内部枚举。
 */
function replyIsAdmin(reply: Partial<V2TicketReply> | undefined | null): boolean {
  if (!reply) return false;
  if (typeof reply.is_admin === "boolean") return reply.is_admin;
  if (reply.author === "admin" || reply.author === "user") return reply.author === "admin";
  return reply.role === BACKEND_ROLE_ADMIN;
}

export function normalizeTicketReply(reply: V2TicketReply): TicketReply {
  const isAdmin = replyIsAdmin(reply);
  return {
    id: reply.id,
    uid: Number(reply?.uid ?? 0),
    username: String(reply?.username ?? ""),
    role: typeof reply?.role === "number" ? reply.role : isAdmin ? BACKEND_ROLE_ADMIN : 1,
    is_admin: isAdmin,
    author: isAdmin ? "admin" : "user",
    content: String(reply?.content ?? ""),
    created_at: Number(reply?.created_at ?? 0),
  };
}

export function normalizeTicketReplies(replies: V2TicketReply[] | null | undefined): TicketReply[] {
  if (!Array.isArray(replies)) return [];
  return replies.map(normalizeTicketReply);
}

export function normalizeTicketAttachment(attachment: V2TicketAttachment): TicketAttachment {
  return {
    filename: String(attachment?.filename ?? ""),
    content_type: String(attachment?.content_type ?? ""),
    size: Number(attachment?.size ?? 0),
    uploaded_uid: Number(attachment?.uploaded_uid ?? 0),
    created_at: Number(attachment?.created_at ?? 0),
    url: String(attachment?.url ?? ""),
  };
}

export function normalizeTicketAttachments(
  attachments: V2TicketAttachment[] | null | undefined,
): TicketAttachment[] {
  if (!Array.isArray(attachments)) return [];
  return attachments.map(normalizeTicketAttachment);
}

/**
 * 把任意一份 V2 工单 DTO（详情 item、回复响应里的 ticket、写操作直接回传的
 * ticket）统一成前端 Ticket。缺少的字段按「空会话」处理，绝不返回 undefined，
 * 免得页面再去写一层判空。
 */
export function normalizeTicket(
  item: Partial<V2UserTicketDetailResponse["item"]> & Partial<V2AdminTicketDetailResponse["item"]>,
): Ticket {
  const source = item ?? {};
  return {
    id: Number(source.id ?? 0),
    revision: source.revision,
    message_page: source.message_page,
    uid: Number(source.uid ?? 0),
    username: String(source.username ?? ""),
    title: String(source.title ?? ""),
    content: String(source.content ?? ""),
    type: String(source.type ?? ""),
    status: pickStatus(source.status),
    priority: pickPriority(source.priority),
    admin_note: typeof source.admin_note === "string" ? source.admin_note : undefined,
    replies: normalizeTicketReplies(source.replies as V2TicketReply[] | undefined),
    attachments: normalizeTicketAttachments(source.attachments as V2TicketAttachment[] | undefined),
    // 有界详情带总数；仅旧完整详情退回数组长度。
    reply_count: source.reply_count ?? (Array.isArray(source.replies) ? source.replies.length : 0),
    attachment_count: Array.isArray(source.attachments) ? source.attachments.length : 0,
    notify_telegram: source.notify_telegram !== false,
    created_at: Number(source.created_at ?? 0),
    updated_at: Number(source.updated_at ?? 0),
    resolved_at: pickTimestamp(source.resolved_at),
    closed_at: pickTimestamp(source.closed_at),
  };
}

export function normalizeUserTicketListItem(
  item: V2UserTicketListResponse["items"][number],
): UserTicketListItem {
  return {
    id: Number(item?.id ?? 0),
    title: String(item?.title ?? ""),
    type: String(item?.type ?? ""),
    status: pickStatus(item?.status),
    priority: pickPriority(item?.priority),
    reply_count: Number(item?.reply_count ?? 0),
    attachment_count: Number(item?.attachment_count ?? 0),
    notify_telegram: item?.notify_telegram !== false,
    created_at: Number(item?.created_at ?? 0),
    updated_at: Number(item?.updated_at ?? 0),
    resolved_at: pickTimestamp(item?.resolved_at),
    closed_at: pickTimestamp(item?.closed_at),
  };
}

export function normalizeAdminTicketListItem(
  item: V2AdminTicketListResponse["items"][number],
): Ticket {
  return {
    id: Number(item?.id ?? 0),
    uid: Number(item?.uid ?? 0),
    username: String(item?.username ?? ""),
    title: String(item?.title ?? ""),
    content: String(item?.content ?? ""),
    type: String(item?.type ?? ""),
    status: pickStatus(item?.status),
    priority: pickPriority(item?.priority),
    admin_note: typeof item?.admin_note === "string" ? item.admin_note : undefined,
    reply_count: Number(item?.reply_count ?? 0),
    attachment_count: Number(item?.attachment_count ?? 0),
    notify_telegram: item?.notify_telegram !== false,
    created_at: Number(item?.created_at ?? 0),
    updated_at: Number(item?.updated_at ?? 0),
    resolved_at: pickTimestamp(item?.resolved_at),
    closed_at: pickTimestamp(item?.closed_at),
  };
}

/** 会话气泡按作者分边时使用；与 normalizeTicketReply 共用同一套判定。 */
export function isTicketReplyFromAdmin(reply: Pick<TicketReply, "is_admin"> | undefined | null): boolean {
  return reply?.is_admin === true;
}

export function mergeTicketResponse(current: Ticket | null, incoming: Ticket): Ticket | null {
  if (!current || current.id !== incoming.id) return current;
  if ((current.revision ?? 0) > (incoming.revision ?? 0)) return current;
  return incoming;
}
