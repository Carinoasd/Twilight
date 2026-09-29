package store

// 读 API 在释放读锁后把结构体副本交给调用方（HTTP handler 常在锁外做 JSON 编码）。
// Go 的结构体值拷贝只复制 slice/map 头，底层数组与 map 仍与 s.state 共用；一旦写者
// 在写锁下就地修改（append(x[:i], x[i+1:]...)、写 map 键），锁外的读者就会与之
// data race，map 并发读写更会让 runtime 直接 fatal。这里集中提供深拷贝，读路径
// 回传前使用；写路径也遵循「改前先复制」（见 RemoveTicketAttachment、
// markSchedulerRunInterrupted）。
//
// User/APIKey/Signin 的引用字段（SeenAnnouncementIDs、LegacyPermissions、
// PendingEmbyDays、Permissions、Records）在 store 内只会整体替换或在末尾 append，
// 不会就地改写已有元素，锁外副本读到的 [0,len) 区间不会被改动，故不做深拷贝以免
// 给最热的用户读路径加分配；新增写路径时必须保持这个约定。

// cloneSlice 复制 slice 并保留 nil 与空 slice 的区别（JSON 输出 null / [] 不变）。
func cloneSlice[T any](in []T) []T {
	if in == nil {
		return nil
	}
	out := make([]T, len(in))
	copy(out, in)
	return out
}

func cloneTicket(t Ticket) Ticket {
	t.Replies = cloneSlice(t.Replies)
	t.Attachments = cloneSlice(t.Attachments)
	if t.NotifyTelegram != nil {
		v := *t.NotifyTelegram
		t.NotifyTelegram = &v
	}
	return t
}

func cloneRegCode(rc RegCode) RegCode {
	rc.UsedByUIDs = cloneSlice(rc.UsedByUIDs)
	rc.UsedByTelegramIDs = cloneSlice(rc.UsedByTelegramIDs)
	return rc
}

func cloneSchedulerRun(run SchedulerRun) SchedulerRun {
	run.Params = cloneSchedulerMap(run.Params)
	run.Summary = cloneSchedulerMap(run.Summary)
	run.Logs = cloneSlice(run.Logs)
	return run
}

func cloneMediaRequest(r MediaRequest) MediaRequest {
	if r.MediaInfo != nil {
		info := make(map[string]any, len(r.MediaInfo))
		for k, v := range r.MediaInfo {
			info[k] = v
		}
		r.MediaInfo = info
	}
	return r
}
