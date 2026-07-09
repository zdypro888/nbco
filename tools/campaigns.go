package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/zdypro888/nbco/ai"
	"github.com/zdypro888/nbco/perm"
	"github.com/zdypro888/nbco/store"
)

func campaignTools(d Deps, u *store.User) []ai.Tool {
	return []ai.Tool{
		tool("create_data_collection_campaign", "创建资料收集活动：向自己、指定成员或全体真人员工收集动态字段（如手机、职位、组别），系统跟踪每个目标缺哪些字段、是否完成，并可继续提醒。需要给他人/全体发送时具备 send_msg 权限；字段定义会自动补齐。",
			obj(map[string]any{
				"title":           p("string", "活动标题，如「全员完善个人档案」"),
				"required_fields": arr("string", "需要收集的字段名，如 手机/职位/组别；常见别名会归一"),
				"instruction":     p("string", "补充说明，可选"),
				"target":          p("string", "self（默认）| _all（全体真人员工，不含 AI worker、不含发起人）| 用户ID/唯一姓名/tg:<Telegram ID>"),
				"user_ids":        arr("integer", "可选，显式目标用户ID列表；提供后优先于 target"),
				"message":         p("string", "发送给目标成员的说明，可选；为空则自动生成"),
				"send_now":        p("boolean", "是否立即通知目标；默认 true"),
			}, "title", "required_fields"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Title          string   `json:"title"`
					RequiredFields []string `json:"required_fields"`
					Instruction    string   `json:"instruction"`
					Target         string   `json:"target"`
					UserIDs        []int64  `json:"user_ids"`
					Message        string   `json:"message"`
					SendNow        *bool    `json:"send_now"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				return createDataCampaign(ctx, d, u, args.Title, args.Instruction, args.RequiredFields, args.Target, args.UserIDs, args.Message, args.SendNow)
			}),

		tool("list_data_collection_campaigns", "查看资料收集活动列表和完成率。用于确认「全员完善资料」这类事项是否真的创建、还有多少人未完成。",
			obj(map[string]any{
				"status": p("string", "active（默认）| closed | cancelled | all"),
				"limit":  p("integer", "返回数量，默认 20，最多 100"),
			}),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					Status string `json:"status"`
					Limit  int    `json:"limit"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				items, err := d.Store.ListDataCollectionCampaigns(ctx, u.ID, u.IsSuperadmin, args.Status, args.Limit)
				if err != nil {
					return "", err
				}
				return renderDataCampaignList(items), nil
			}),

		tool("get_data_collection_campaign", "查看某个资料收集活动的目标明细、缺失字段和完成状态；查看前会刷新完成率。",
			obj(map[string]any{"campaign_id": p("integer", "资料收集活动内部编号")}, "campaign_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					CampaignID int64 `json:"campaign_id"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				return getDataCampaign(ctx, d, u, args.CampaignID)
			}),

		tool("send_data_collection_reminder", "给资料收集活动中仍未完成的目标重发提醒。只提醒 pending 目标；发送成功后记录 last_notified_at。",
			obj(map[string]any{
				"campaign_id": p("integer", "资料收集活动内部编号"),
				"message":     p("string", "可选，覆盖默认提醒文案"),
			}, "campaign_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					CampaignID int64  `json:"campaign_id"`
					Message    string `json:"message"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				return remindDataCampaign(ctx, d, u, args.CampaignID, args.Message)
			}),

		tool("close_data_collection_campaign", "关闭或取消一个资料收集活动；历史状态保留，不再进入 active 列表。",
			obj(map[string]any{
				"campaign_id": p("integer", "资料收集活动内部编号"),
				"status":      p("string", "closed 或 cancelled；默认 closed"),
			}, "campaign_id"),
			func(ctx context.Context, raw json.RawMessage) (string, error) {
				var args struct {
					CampaignID int64  `json:"campaign_id"`
					Status     string `json:"status"`
				}
				if err := decode(raw, &args); err != nil {
					return err.Error(), nil
				}
				return closeDataCampaign(ctx, d, u, args.CampaignID, args.Status)
			}),
	}
}

func createDataCampaign(ctx context.Context, d Deps, u *store.User, title, instruction string, fields []string, target string, userIDs []int64, message string, sendNowArg *bool) (string, error) {
	if d.Store == nil {
		return "资料收集活动需要可用的存储服务。", nil
	}
	if u == nil {
		return "当前用户不可用。", nil
	}
	title = strings.TrimSpace(title)
	if title == "" {
		return "title 不能为空。", nil
	}
	required := canonicalDataFields(fields)
	if len(required) == 0 {
		return "required_fields 不能为空。", nil
	}
	targetIDs, msg, err := resolveDataCampaignTargets(ctx, d, u, target, userIDs)
	if err != nil || msg != "" {
		return msg, err
	}
	if len(targetIDs) == 0 {
		return "没有可收集的目标成员。", nil
	}
	if err := d.Store.EnsureInfoFields(ctx, required); err != nil {
		return "", err
	}
	c, err := d.Store.CreateDataCollectionCampaign(ctx, title, instruction, required, u.ID, targetIDs)
	if err != nil {
		return "", err
	}
	if _, err := d.Store.RefreshDataCollectionCampaign(ctx, c.ID); err != nil {
		return "", err
	}
	sendNow := true
	if sendNowArg != nil {
		sendNow = *sendNowArg
	}
	targets, err := d.Store.DataCollectionCampaignTargets(ctx, c.ID)
	if err != nil {
		return "", err
	}
	sent, failed := 0, 0
	var failedNames []string
	if sendNow {
		sent, failed, failedNames, err = notifyDataCampaignTargets(ctx, d, u, c, targets, message, true)
		if err != nil {
			return "", err
		}
	}
	view, err := dataCampaignView(ctx, d, u, c.ID)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "已创建资料收集活动（%s）：%s。\n", internalRef("资料收集", c.ID), c.Title)
	fmt.Fprintf(&b, "字段：%s\n目标：%d 人，已完成 %d，待补 %d。", strings.Join(c.RequiredFields, "、"), view.Total, view.Completed, view.Pending)
	if sendNow {
		fmt.Fprintf(&b, "\n通知发送：成功 %d 人，失败 %d 人", sent, failed)
		if len(failedNames) > 0 {
			fmt.Fprintf(&b, "（失败示例：%s）", strings.Join(failedNames, "、"))
		}
		b.WriteString("。")
	} else {
		b.WriteString("\n本次未发送通知；可用 send_data_collection_reminder 后续提醒。")
	}
	return b.String(), nil
}

func getDataCampaign(ctx context.Context, d Deps, u *store.User, id int64) (string, error) {
	if id <= 0 {
		return "campaign_id 必须是资料收集活动内部编号。", nil
	}
	if _, err := d.Store.RefreshDataCollectionCampaign(ctx, id); err != nil {
		return "", err
	}
	c, err := d.Store.DataCollectionCampaignByID(ctx, id)
	if err != nil {
		return "资料收集活动不存在。", nil
	}
	targets, err := d.Store.DataCollectionCampaignTargets(ctx, id)
	if err != nil {
		return "", err
	}
	if !canSeeDataCampaign(u, c, targets) {
		return "你无权查看该资料收集活动。", nil
	}
	return renderDataCampaignDetail(c, targets), nil
}

func remindDataCampaign(ctx context.Context, d Deps, u *store.User, id int64, message string) (string, error) {
	if _, err := d.Store.RefreshDataCollectionCampaign(ctx, id); err != nil {
		return "", err
	}
	c, err := d.Store.DataCollectionCampaignByID(ctx, id)
	if err != nil {
		return "资料收集活动不存在。", nil
	}
	if !canManageDataCampaign(u, c) {
		return "只有创建者或超级管理员可以提醒该资料收集活动。", nil
	}
	if c.Status != store.DataCampaignActive {
		return "该资料收集活动不是 active 状态，不能继续提醒。", nil
	}
	targets, err := d.Store.DataCollectionCampaignTargets(ctx, id)
	if err != nil {
		return "", err
	}
	var pending []store.DataCollectionCampaignTarget
	for _, t := range targets {
		if t.Status == store.DataCampaignTargetPending {
			if msg, err := canSendToDataCampaignTarget(ctx, d, u, t.UserID); err != nil || msg != "" {
				return msg, err
			}
			pending = append(pending, t)
		}
	}
	if len(pending) == 0 {
		return "所有目标都已完成，无需提醒。", nil
	}
	sent, failed, failedNames, err := notifyDataCampaignTargets(ctx, d, u, c, pending, message, false)
	if err != nil {
		return "", err
	}
	if len(failedNames) > 0 {
		return fmt.Sprintf("已提醒资料收集活动（%s）的待补目标：成功 %d 人，失败 %d 人（失败示例：%s）。",
			internalRef("资料收集", id), sent, failed, strings.Join(failedNames, "、")), nil
	}
	return fmt.Sprintf("已提醒资料收集活动（%s）的待补目标：成功 %d 人，失败 %d 人。", internalRef("资料收集", id), sent, failed), nil
}

func closeDataCampaign(ctx context.Context, d Deps, u *store.User, id int64, status string) (string, error) {
	c, err := d.Store.DataCollectionCampaignByID(ctx, id)
	if err != nil {
		return "资料收集活动不存在。", nil
	}
	if !canManageDataCampaign(u, c) {
		return "只有创建者或超级管理员可以关闭该资料收集活动。", nil
	}
	status = strings.TrimSpace(status)
	if status == "" {
		status = store.DataCampaignClosed
	}
	if status != store.DataCampaignClosed && status != store.DataCampaignCancelled {
		return "status 必须是 closed 或 cancelled。", nil
	}
	if err := d.Store.SetDataCollectionCampaignStatus(ctx, id, status); err != nil {
		return "", err
	}
	return fmt.Sprintf("已将资料收集活动（%s）设置为 %s。", internalRef("资料收集", id), status), nil
}

func resolveDataCampaignTargets(ctx context.Context, d Deps, u *store.User, target string, userIDs []int64) ([]int64, string, error) {
	seen := map[int64]bool{}
	var ids []int64
	add := func(id int64) {
		if id > 0 && !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	if len(userIDs) > 0 {
		for _, id := range userIDs {
			tu, err := mustUser(ctx, d.Store, id)
			if err != nil {
				return nil, err.Error(), nil
			}
			if tu.IsWorker {
				return nil, "资料收集目标必须是真人员工，不能是 AI worker。", nil
			}
			if msg, err := canSendToDataCampaignTarget(ctx, d, u, tu.ID); err != nil || msg != "" {
				return nil, msg, err
			}
			add(tu.ID)
		}
		return ids, "", nil
	}
	target = strings.TrimSpace(target)
	switch {
	case target == "" || strings.EqualFold(target, "self"):
		if u == nil || u.ID <= 0 {
			return nil, "当前用户不可用。", nil
		}
		add(u.ID)
	case target == store.TargetAll:
		if !u.IsSuperadmin && !hasActiveAll(ctx, d, u.ID, perm.ActSendMsg) {
			return nil, "给全体创建资料收集活动需要 send_msg:_all 权限。", nil
		}
		users, err := d.Store.ListUsers(ctx)
		if err != nil {
			return nil, "", err
		}
		for _, tu := range users {
			if tu == nil || tu.Status != store.UserActive || tu.IsWorker || tu.ID == u.ID {
				continue
			}
			add(tu.ID)
		}
	default:
		tu, msg, err := resolveUserArg(ctx, d.Store, 0, target)
		if err != nil || msg != "" {
			return nil, msg, err
		}
		if tu.IsWorker {
			return nil, "资料收集目标必须是真人员工，不能是 AI worker。", nil
		}
		if msg, err := canSendToDataCampaignTarget(ctx, d, u, tu.ID); err != nil || msg != "" {
			return nil, msg, err
		}
		add(tu.ID)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	return ids, "", nil
}

func canSendToDataCampaignTarget(ctx context.Context, d Deps, u *store.User, targetID int64) (string, error) {
	if u == nil {
		return "当前用户不可用。", nil
	}
	if targetID == u.ID || u.IsSuperadmin {
		return "", nil
	}
	grants, err := d.Store.PermsOf(ctx, u.ID)
	if err != nil {
		return "", err
	}
	if !perm.CheckActive(grants, perm.ActSendMsg, targetID) {
		return "你没有对该用户的 send_msg 权限。", nil
	}
	return "", nil
}

func notifyDataCampaignTargets(ctx context.Context, d Deps, sender *store.User, c *store.DataCollectionCampaign, targets []store.DataCollectionCampaignTarget, custom string, initial bool) (int, int, []string, error) {
	if d.Notifier == nil {
		pending := 0
		var names []string
		for _, t := range targets {
			if t.Status != store.DataCampaignTargetCompleted {
				pending++
				if len(names) < 5 {
					names = append(names, dataCampaignTargetName(t))
				}
			}
		}
		return 0, pending, names, nil
	}
	sentIDs := make([]int64, 0, len(targets))
	failed := 0
	var failedNames []string
	for _, t := range targets {
		if t.Status == store.DataCampaignTargetCompleted {
			continue
		}
		body := dataCampaignMessage(sender, c, t, custom, initial)
		if err := d.Notifier.Send(ctx, t.UserID, body); err != nil {
			failed++
			if len(failedNames) < 5 {
				failedNames = append(failedNames, dataCampaignTargetName(t))
			}
			continue
		}
		sentIDs = append(sentIDs, t.UserID)
	}
	if err := d.Store.MarkDataCollectionCampaignTargetsNotified(ctx, c.ID, sentIDs); err != nil {
		return len(sentIDs), failed, failedNames, err
	}
	return len(sentIDs), failed, failedNames, nil
}

func dataCampaignTargetName(t store.DataCollectionCampaignTarget) string {
	name := strings.TrimSpace(t.UserName)
	if name == "" {
		return fmt.Sprintf("员工ID %d", t.UserID)
	}
	return name
}

func dataCampaignMessage(sender *store.User, c *store.DataCollectionCampaign, t store.DataCollectionCampaignTarget, custom string, initial bool) string {
	custom = strings.TrimSpace(custom)
	var b strings.Builder
	if custom != "" {
		b.WriteString(custom)
	} else if initial {
		fmt.Fprintf(&b, "📋 %s\n请在和 nbco 的私聊中补充以下资料：%s。", c.Title, strings.Join(t.MissingFields, "、"))
	} else {
		fmt.Fprintf(&b, "📋 %s 还差这些资料：%s。请在和 nbco 的私聊中补充。", c.Title, strings.Join(t.MissingFields, "、"))
	}
	if strings.TrimSpace(c.Instruction) != "" {
		fmt.Fprintf(&b, "\n说明：%s", c.Instruction)
	}
	if sender != nil && sender.Name != "" {
		fmt.Fprintf(&b, "\n发起人：%s", sender.Name)
	}
	return b.String()
}

func canonicalDataFields(fields []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range fields {
		name := defaultInfoFieldCanonical(f)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

func canSeeDataCampaign(u *store.User, c *store.DataCollectionCampaign, targets []store.DataCollectionCampaignTarget) bool {
	if u == nil || c == nil {
		return false
	}
	if u.IsSuperadmin || c.CreatedBy == u.ID {
		return true
	}
	for _, t := range targets {
		if t.UserID == u.ID {
			return true
		}
	}
	return false
}

func canManageDataCampaign(u *store.User, c *store.DataCollectionCampaign) bool {
	return u != nil && c != nil && (u.IsSuperadmin || c.CreatedBy == u.ID)
}

func dataCampaignView(ctx context.Context, d Deps, u *store.User, id int64) (store.DataCollectionCampaignView, error) {
	items, err := d.Store.ListDataCollectionCampaigns(ctx, u.ID, u.IsSuperadmin, "all", 200)
	if err != nil {
		return store.DataCollectionCampaignView{}, err
	}
	for _, it := range items {
		if it.ID == id {
			return it, nil
		}
	}
	return store.DataCollectionCampaignView{}, store.ErrNotFound
}

func renderDataCampaignList(items []store.DataCollectionCampaignView) string {
	if len(items) == 0 {
		return "（没有资料收集活动）"
	}
	var b strings.Builder
	b.WriteString("资料收集活动\n")
	for _, it := range items {
		creator := it.CreatorName
		if creator == "" {
			creator = strconv.FormatInt(it.CreatedBy, 10)
		}
		fmt.Fprintf(&b, "- %s：%s [%s] 完成 %d/%d，待补 %d，创建者 %s\n",
			internalRef("资料收集", it.ID), it.Title, it.Status, it.Completed, it.Total, it.Pending, creator)
	}
	return strings.TrimSpace(b.String())
}

func renderDataCampaignDetail(c *store.DataCollectionCampaign, targets []store.DataCollectionCampaignTarget) string {
	var completed, pending int
	for _, t := range targets {
		switch t.Status {
		case store.DataCampaignTargetCompleted:
			completed++
		case store.DataCampaignTargetPending:
			pending++
		}
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%s：%s [%s]\n", internalRef("资料收集", c.ID), c.Title, c.Status)
	fmt.Fprintf(&b, "字段：%s\n完成：%d/%d，待补：%d\n", strings.Join(c.RequiredFields, "、"), completed, len(targets), pending)
	if strings.TrimSpace(c.Instruction) != "" {
		fmt.Fprintf(&b, "说明：%s\n", c.Instruction)
	}
	for _, t := range targets {
		if t.Status == store.DataCampaignTargetCompleted {
			fmt.Fprintf(&b, "- 员工ID %d｜%s｜已完成\n", t.UserID, t.UserName)
			continue
		}
		fmt.Fprintf(&b, "- 员工ID %d｜%s｜待补：%s\n", t.UserID, t.UserName, strings.Join(t.MissingFields, "、"))
	}
	return strings.TrimSpace(b.String())
}
