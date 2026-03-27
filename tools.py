from datetime import datetime, timezone
from claude_agent_sdk import tool, create_sdk_mcp_server
from db import get_db
import auth
import key
import permission
import profile
import task
import role
import notify
import scheduler
import api_token
import fields

_INTERNAL_FIELDS = {"_id", "infos", "active_perms", "passive_perms", "session_id", "api_token", "api_token_created_at"}


def _profile_projection() -> dict:
    """动态生成排除内部字段的 projection"""
    return {f: 0 for f in _INTERNAL_FIELDS}


def _fmt_task(t: dict) -> str:
    cl = t.get("checklist", [])
    done = sum(1 for c in cl if c.get("done"))
    progress = f" ({done}/{len(cl)})" if cl else ""
    extra = ""
    if t.get("split_into"):
        assignees = [str(c["assignee_tg_id"]) for c in t["split_into"]]
        extra = f" → 拆分给 {', '.join(assignees)}"
    return f"[{t['status']}] {t['title']}{progress} (id:{t['_id']}){extra}"


def _fmt_tree(node, indent=0):
    prefix = "  " * indent
    lines = [f"{prefix}[{node['status']}] {node['title']} → {node['assignee_tg_id']} ({node['progress']})"]
    for child in node.get("children", []):
        lines.extend(_fmt_tree(child, indent + 1))
    return lines


# ===== 公共工具方法 =====

def _ok(text: str) -> dict:
    return {"content": [{"type": "text", "text": text}]}


def _err(text: str) -> dict:
    return {"content": [{"type": "text", "text": text}], "is_error": True}


def _parse_target(value: str) -> int | str:
    """解析 tg_id 或 '_all'，非法值抛 ValueError"""
    if value == "_all":
        return "_all"
    if not value.isdigit():
        raise ValueError(f"无效的 ID: {value}")
    return int(value)


async def _user_exists(tg_id: int) -> bool:
    return await get_db().users.find_one({"tg_id": tg_id}) is not None


# ===== User Tools =====

def create_user_tools(tg_id: int, is_superadmin: bool = False):

    @tool("get_my_profile", "查询自己的个人信息（不含画像）。",
          {"type": "object", "properties": {}, "required": []})
    async def get_my_profile(args: dict) -> dict:
        doc = await get_db().users.find_one({"tg_id": tg_id}, _profile_projection())
        if doc is None:
            return _ok("你尚未录入个人信息。")
        return _ok(str(doc))

    @tool("update_my_profile", f"更新自己的基本信息。可更新字段：{', '.join(fields.get_fields())}。",
          {"type": "object", "properties": {"fields": {"type": "object"}}, "required": ["fields"]})
    async def update_my_profile(args: dict) -> dict:
        valid_fields = set(fields.get_fields())
        update_data = {k: v for k, v in args["fields"].items() if k in valid_fields and isinstance(v, str)}
        if not update_data:
            return _err("没有可更新的有效字段。")
        update_data["updated_at"] = datetime.now(timezone.utc)
        await get_db().users.update_one(
            {"tg_id": tg_id},
            {"$set": update_data, "$setOnInsert": {"tg_id": tg_id, "created_at": datetime.now(timezone.utc)}},
            upsert=True,
        )
        auth.invalidate(tg_id)
        doc = await get_db().users.find_one({"tg_id": tg_id}, _profile_projection())
        return _ok(f"已更新：{doc}")

    @tool("get_my_infos", "查看自己的自我介绍列表。",
          {"type": "object", "properties": {}, "required": []})
    async def get_my_infos(args: dict) -> dict:
        items = await profile.get_by_author(tg_id, tg_id)
        return _ok(str(items)) if items else _ok("你还没有写自我介绍。")

    @tool("save_my_infos", "保存自己的自我介绍列表（整体替换）。",
          {"type": "object", "properties": {"items": {"type": "array", "items": {"type": "string"}}}, "required": ["items"]})
    async def save_my_infos(args: dict) -> dict:
        items = [s.strip() for s in args["items"] if isinstance(s, str) and s.strip()]
        if not items:
            return _err("列表为空，未保存。")
        await profile.replace(tg_id, tg_id, items)
        return _ok("已保存。")

    @tool("list_my_active_perms", "查看我能对谁做什么（我的主动权限）。",
          {"type": "object", "properties": {}, "required": []})
    async def list_my_active_perms(args: dict) -> dict:
        if is_superadmin:
            return _ok("你是超级管理员，拥有所有主动权限。")
        perms = await permission.list_active(tg_id)
        if not perms:
            return _ok("没有任何主动权限。")
        lines = [f"对 {k}: {', '.join(v)}" for k, v in perms.items()]
        return _ok("\n".join(lines))

    @tool("list_my_passive_perms", "查看谁能对我做什么（我的被动权限）。",
          {"type": "object", "properties": {}, "required": []})
    async def list_my_passive_perms(args: dict) -> dict:
        perms = await permission.list_passive(tg_id)
        if not perms:
            return _ok("没有任何被动权限。")
        lines = [f"允许 {k}: {', '.join(v)}" for k, v in perms.items()]
        return _ok("\n".join(lines))

    @tool("grant_my_passive_perm", "允许某人查看我身上的画像。action: view_profile:作者tg_id 或 view_profile:_all。",
          {"type": "object", "properties": {
              "viewer_tg_id": {"type": "string", "description": "tg_id 或 '_all'"},
              "action": {"type": "string"},
          }, "required": ["viewer_tg_id", "action"]})
    async def grant_my_passive_perm(args: dict) -> dict:
        if not permission.is_valid_passive_action(args["action"]):
            return _err("无效的权限格式。需为 view_profile:tg_id 或 view_profile:_all")
        try:
            viewer = _parse_target(args["viewer_tg_id"])
        except ValueError as e:
            return _err(str(e))
        await permission.grant_passive(tg_id, args["action"], viewer)
        return _ok("已授权。")

    @tool("revoke_my_passive_perm", "撤销某人查看我身上画像的权限。",
          {"type": "object", "properties": {
              "viewer_tg_id": {"type": "string", "description": "tg_id 或 '_all'"},
              "action": {"type": "string"},
          }, "required": ["viewer_tg_id", "action"]})
    async def revoke_my_passive_perm(args: dict) -> dict:
        try:
            viewer = _parse_target(args["viewer_tg_id"])
        except ValueError as e:
            return _err(str(e))
        await permission.revoke_passive(tg_id, args["action"], viewer)
        return _ok("已撤销。")

    async def _check_my_task(task_id: str) -> tuple[dict | None, str | None]:
        """校验任务属于自己，返回 (task, error)"""
        t = await task.get_task(task_id)
        if not t or t["assignee_tg_id"] != tg_id:
            return None, "任务不存在或不属于你。"
        return t, None

    # ----- 我的任务 -----

    @tool("get_my_projects", "查看我参与的项目。",
          {"type": "object", "properties": {}, "required": []})
    async def get_my_projects(args: dict) -> dict:
        tasks = await task.get_my_all_tasks(tg_id)
        if not tasks:
            return _ok("你没有参与任何项目。")
        project_ids = list({str(t["project_id"]) for t in tasks})
        lines = []
        for pid in project_ids:
            p = await task.get_project(pid)
            if p and p["status"] == "active":
                lines.append(f"{p['name']}：{p.get('description', '')[:50]}")
        return _ok("\n".join(lines)) if lines else _ok("你没有参与任何活跃项目。")

    @tool("get_my_tasks", "查看我的待办任务。",
          {"type": "object", "properties": {}, "required": []})
    async def get_my_tasks_tool(args: dict) -> dict:
        tasks = await task.get_my_tasks(tg_id)
        if not tasks:
            return _ok("你当前没有待办任务。")
        lines = [_fmt_task(t) for t in tasks]
        return _ok("\n".join(lines))

    @tool("get_my_all_tasks", "查看我的所有任务（含已完成和已拆分）。",
          {"type": "object", "properties": {}, "required": []})
    async def get_my_all_tasks_tool(args: dict) -> dict:
        tasks = await task.get_my_all_tasks(tg_id)
        if not tasks:
            return _ok("你没有任何任务记录。")
        lines = [_fmt_task(t) for t in tasks]
        return _ok("\n".join(lines))

    @tool("get_task_detail", "查看我的某个任务详情（含描述、清单、进度日志）。",
          {"type": "object", "properties": {"task_id": {"type": "string"}}, "required": ["task_id"]})
    async def get_task_detail(args: dict) -> dict:
        t, err = await _check_my_task(args["task_id"])
        if err:
            return _err(err)
        # 关联项目信息
        project = await task.get_project(str(t["project_id"]))
        project_info = f"{project['name']}：{project['description']}" if project else "未知项目"
        lines = [f"所属项目：{project_info}", f"任务：{t['title']}", f"目标：{t.get('goal', '无')}", f"描述：{t['description']}", f"状态：{t['status']}"]
        cl = t.get("checklist", [])
        if cl:
            lines.append("清单：")
            for i, c in enumerate(cl):
                mark = "✅" if c["done"] else "☐"
                lines.append(f"  {mark} [{i}] {c['item']}")
        notes = t.get("progress_notes", [])
        if notes:
            lines.append("进度日志：")
            for n in notes[-5:]:
                lines.append(f"  {n['time'].strftime('%m-%d %H:%M')}: {n['content']}")
        attachments = t.get("attachments", [])
        if attachments:
            lines.append(f"附件：{len(attachments)} 张图片")
        return _ok("\n".join(lines))

    @tool("view_my_task_tree", "查看我的某个任务的完整拆分树。",
          {"type": "object", "properties": {"task_id": {"type": "string"}}, "required": ["task_id"]})
    async def view_my_task_tree(args: dict) -> dict:
        t, err = await _check_my_task(args["task_id"])
        if err:
            return _err(err)
        tree = await task.get_task_tree(args["task_id"])
        if not tree:
            return _err("获取失败。")
        return _ok("\n".join(_fmt_tree(tree)))

    @tool("update_my_task_status", "更新我的某个任务状态。",
          {"type": "object", "properties": {
              "task_id": {"type": "string"},
              "status": {"type": "string", "enum": ["pending", "in_progress", "done", "cancelled"]},
          }, "required": ["task_id", "status"]})
    async def update_my_task_status(args: dict) -> dict:
        t, err = await _check_my_task(args["task_id"])
        if err:
            return _err(err)
        if t["status"] == "split":
            return _err("已拆分的任务不能更改状态。")
        ok = await task.update_status(args["task_id"], args["status"])
        if ok:
            await notify.task_status_changed(t["assigner_tg_id"], t["title"], tg_id, args["status"])
            return _ok(f"已更新为 {args['status']}。")
        return _err("更新失败。")

    # ----- Checklist -----

    @tool("save_checklist", "保存任务的工作清单（整体替换）。AI 根据任务描述归纳生成。",
          {"type": "object", "properties": {
              "task_id": {"type": "string"},
              "items": {"type": "array", "items": {"type": "string"}, "description": "清单条目列表"},
          }, "required": ["task_id", "items"]})
    async def save_checklist(args: dict) -> dict:
        t, err = await _check_my_task(args["task_id"])
        if err:
            return _err(err)
        checklist = [{"item": s.strip(), "done": False} for s in args["items"] if isinstance(s, str) and s.strip()]
        if not checklist:
            return _err("清单为空。")
        await task.set_checklist(args["task_id"], checklist)
        return _ok(f"已保存 {len(checklist)} 条清单。")

    @tool("toggle_checklist", "勾选或取消勾选清单条目。",
          {"type": "object", "properties": {
              "task_id": {"type": "string"},
              "index": {"type": "integer"},
              "done": {"type": "boolean"},
          }, "required": ["task_id", "index", "done"]})
    async def toggle_checklist(args: dict) -> dict:
        t, err = await _check_my_task(args["task_id"])
        if err:
            return _err(err)
        ok = await task.toggle_checklist_item(args["task_id"], args["index"], args["done"])
        return _ok("已更新。") if ok else _err("序号不存在。")

    # ----- Progress Notes -----

    @tool("add_progress", "给任务添加进度记录。AI 根据用户汇报自动总结。",
          {"type": "object", "properties": {
              "task_id": {"type": "string"},
              "content": {"type": "string"},
          }, "required": ["task_id", "content"]})
    async def add_progress(args: dict) -> dict:
        t, err = await _check_my_task(args["task_id"])
        if err:
            return _err(err)
        content = args["content"].strip()
        if not content:
            return _err("内容为空。")
        await task.add_progress_note(args["task_id"], content)
        return _ok("进度已记录。")

    # ----- 分配出去的任务 -----

    @tool("get_assigned_tasks", "查看我分配出去的任务及其进度。",
          {"type": "object", "properties": {}, "required": []})
    async def get_assigned_tasks(args: dict) -> dict:
        tasks = await task.get_assigned_by_me(tg_id)
        if not tasks:
            return _ok("你没有分配出去的任务。")
        lines = []
        for t in tasks:
            cl = t.get("checklist", [])
            done = sum(1 for c in cl if c.get("done"))
            progress = f" ({done}/{len(cl)})" if cl else ""
            lines.append(f"[{t['status']}] {t['title']} → {t['assignee_tg_id']}{progress}")
            notes = t.get("progress_notes", [])
            if notes:
                latest = notes[-1]
                lines.append(f"  最新进度: {latest['content']}")
        return _ok("\n".join(lines))

    @tool("update_assigned_task", "修改我分配出去的任务的目标和/或描述。只有分配者能改。",
          {"type": "object", "properties": {
              "task_id": {"type": "string"},
              "goal": {"type": "string", "description": "新目标（可选）"},
              "description": {"type": "string", "description": "新描述（可选）"},
          }, "required": ["task_id"]})
    async def update_assigned_task(args: dict) -> dict:
        t = await task.get_task(args["task_id"])
        if not t or t["assigner_tg_id"] != tg_id:
            return _err("任务不存在或你不是分配者。")
        goal = args.get("goal", "").strip() or None
        desc = args.get("description", "").strip() or None
        if not goal and not desc:
            return _err("至少提供 goal 或 description。")
        await task.update_task_fields(args["task_id"], goal=goal, description=desc)
        await notify.task_desc_changed(t["assignee_tg_id"], t["title"])
        return _ok("已更新。")

    # ----- 拆分 -----

    @tool("split_my_task", "拆分我的任务并分配给其他人（也可以分给自己）。原任务标记为已拆分。",
          {"type": "object", "properties": {
              "task_id": {"type": "string"},
              "subtasks": {"type": "array", "items": {"type": "object", "properties": {
                  "title": {"type": "string"},
                  "goal": {"type": "string", "description": "子任务目标"},
                  "description": {"type": "string", "description": "子任务描述"},
                  "assignee_tg_id": {"type": "integer"},
              }, "required": ["title", "goal", "description", "assignee_tg_id"]}},
          }, "required": ["task_id", "subtasks"]})
    async def split_my_task(args: dict) -> dict:
        t, err = await _check_my_task(args["task_id"])
        if err:
            return _err(err)
        if t["status"] in ("split", "done", "cancelled"):
            return _err(f"状态为 {t['status']} 的任务不能拆分。")

        # 校验权限（分给自己不需要权限，分给别人需要能看到对方）
        for sub in args["subtasks"]:
            assignee = sub["assignee_tg_id"]
            if assignee != tg_id:
                if not await permission.check_active(tg_id, "view_self_intro", assignee, is_superadmin):
                    return _err(f"你没有把任务分配给 {assignee} 的权限。")
                if not await _user_exists(assignee):
                    return _err(f"用户 {assignee} 不存在。")

        children_info = []
        for sub in args["subtasks"]:
            child_id = await task.create_task(
                project_id=str(t["project_id"]),
                title=sub["title"], goal=sub["goal"], description=sub["description"],
                assignee_tg_id=sub["assignee_tg_id"], assigner_tg_id=tg_id,
            )
            children_info.append({"task_id": child_id, "assignee_tg_id": sub["assignee_tg_id"]})

        ok = await task.mark_split(args["task_id"], children_info)
        if not ok:
            # 回滚：删除已创建的子任务
            for info in children_info:
                await task.delete_task(info["task_id"])
            return _err("拆分失败，任务状态可能已变化。")
        # 自动继承权限 + 通知
        for sub, info in zip(args["subtasks"], children_info):
            if info["assignee_tg_id"] != tg_id:
                await permission.inherit_view_perms(tg_id, info["assignee_tg_id"], is_superadmin)
                auth.invalidate(info["assignee_tg_id"])
                await notify.task_assigned(info["assignee_tg_id"], sub["title"], sub["goal"], tg_id)
        if t["assigner_tg_id"] != tg_id:
            await notify.task_split(t["assigner_tg_id"], t["title"], tg_id, len(children_info))
        return _ok(f"已拆分为 {len(children_info)} 个子任务。")

    # ----- 删除任务 -----

    @tool("delete_assigned_task", "删除我分配出去的任务（会递归删除其子任务）。只有分配者能删。",
          {"type": "object", "properties": {"task_id": {"type": "string"}}, "required": ["task_id"]})
    async def delete_assigned_task(args: dict) -> dict:
        t = await task.get_task(args["task_id"])
        if not t or t["assigner_tg_id"] != tg_id:
            return _err("任务不存在或你不是分配者。")
        await task.delete_task(args["task_id"])
        return _ok("任务已删除。")

    # ----- 角色/Skill -----

    @tool("attach_to_task", "给任务附加图片。需要提供任务 ID 和图片 file_id。",
          {"type": "object", "properties": {
              "task_id": {"type": "string"},
              "file_id": {"type": "string", "description": "Telegram file_id"},
              "description": {"type": "string", "description": "图片说明（可选）"},
          }, "required": ["task_id", "file_id"]})
    async def attach_to_task(args: dict) -> dict:
        t, err = await _check_my_task(args["task_id"])
        if err:
            # 也允许 assigner 添加附件
            t = await task.get_task(args["task_id"])
            if not t or t["assigner_tg_id"] != tg_id:
                return _err("任务不存在或你无权操作。")
        ok = await task.add_attachment(args["task_id"], args["file_id"], args.get("description", ""))
        return _ok("图片已附加到任务。") if ok else _err("附加失败。")

    # ----- 定时任务 -----

    @tool("schedule_once", "设置单次定时提醒。时间格式：ISO 8601（如 2026-03-28T09:00:00+08:00）。",
          {"type": "object", "properties": {
              "name": {"type": "string", "description": "任务名称（唯一标识）"},
              "message": {"type": "string", "description": "提醒内容"},
              "run_at": {"type": "string", "description": "执行时间 ISO 格式"},
          }, "required": ["name", "message", "run_at"]})
    async def schedule_once_tool(args: dict) -> dict:
        from datetime import datetime as dt
        try:
            run_at = dt.fromisoformat(args["run_at"])
        except ValueError:
            return _err("时间格式无效，需要 ISO 8601 格式。")
        err = await scheduler.schedule_once(tg_id, args["name"], args["message"], run_at, is_superadmin)
        return _ok(f"已设置定时提醒：{args['name']}") if err is None else _err(err)

    @tool("schedule_repeating", "设置循环定时提醒。最小间隔 60 秒。",
          {"type": "object", "properties": {
              "name": {"type": "string", "description": "任务名称（唯一标识）"},
              "message": {"type": "string", "description": "提醒内容"},
              "interval_seconds": {"type": "integer", "description": "间隔秒数"},
          }, "required": ["name", "message", "interval_seconds"]})
    async def schedule_repeating_tool(args: dict) -> dict:
        err = await scheduler.schedule_repeating(tg_id, args["name"], args["message"], args["interval_seconds"], is_superadmin)
        return _ok(f"已设置循环提醒：{args['name']}") if err is None else _err(err)

    @tool("cancel_schedule", "取消一个定时任务。",
          {"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]})
    async def cancel_schedule(args: dict) -> dict:
        ok = await scheduler.cancel_job(tg_id, args["name"])
        return _ok("已取消。") if ok else _err("任务不存在。")

    @tool("list_schedules", "查看我的所有定时任务。",
          {"type": "object", "properties": {}, "required": []})
    async def list_schedules(args: dict) -> dict:
        jobs = scheduler.list_user_jobs(tg_id)
        if not jobs:
            return _ok("没有定时任务。")
        lines = [f"{j['name']}：{j['message']} (下次: {j['next_run']})" for j in jobs]
        return _ok("\n".join(lines))

    @tool("activate_role", "激活一个角色/Skill。激活后 AI 将按该角色的思维方式工作。",
          {"type": "object", "properties": {
              "name": {"type": "string", "description": "角色名称"},
          }, "required": ["name"]})
    async def activate_role(args: dict) -> dict:
        r = role.get_by_name(args["name"])
        if not r:
            return _err(f"角色 '{args['name']}' 不存在。")
        return _ok(f"已激活角色：{r['name']}\n\n{r['prompt']}")

    # ----- API Token -----

    @tool("generate_api_token", "生成 API Token，用于 HTTP MCP 接口认证。会替换旧 token。",
          {"type": "object", "properties": {}, "required": []})
    async def generate_api_token(args: dict) -> dict:
        token = await api_token.generate(tg_id)
        return _ok(f"你的 API Token：\n<code>{token}</code>\n请妥善保管，不要泄露。")

    @tool("revoke_api_token", "撤销 API Token。",
          {"type": "object", "properties": {}, "required": []})
    async def revoke_api_token(args: dict) -> dict:
        ok = await api_token.revoke(tg_id)
        return _ok("API Token 已撤销。") if ok else _err("没有有效的 Token。")

    _user_tool_list = [
        get_my_profile, update_my_profile, get_my_infos, save_my_infos,
        list_my_active_perms, list_my_passive_perms, grant_my_passive_perm, revoke_my_passive_perm,
        get_my_projects, get_my_tasks_tool, get_my_all_tasks_tool, get_task_detail, view_my_task_tree,
        update_my_task_status, save_checklist, toggle_checklist, add_progress,
        get_assigned_tasks, update_assigned_task, split_my_task, delete_assigned_task,
        attach_to_task,
        schedule_once_tool, schedule_repeating_tool, cancel_schedule, list_schedules,
        activate_role, generate_api_token, revoke_api_token,
    ]
    return create_sdk_mcp_server(name="user", version="1.0.0", tools=_user_tool_list), _user_tool_list


# ===== Admin Tools =====

def create_admin_tools(tg_id: int, is_superadmin: bool):

    async def _check_active(action: str, target: int) -> str | None:
        """校验主动权限，返回 None 通过，返回错误信息拒绝"""
        if await permission.check_active(tg_id, action, target, is_superadmin):
            return None
        return "无权限。"

    # ----- Key -----

    @tool("generate_key", "生成绑定 Key（会替换之前的）。",
          {"type": "object", "properties": {}, "required": []})
    async def generate_key_tool(args: dict) -> dict:
        if err := await _check_active("generate_key", tg_id):
            return _err(err)
        code = key.generate()
        return _ok(f"已生成绑定 Key：{code}")

    @tool("cancel_key", "取消当前绑定 Key。",
          {"type": "object", "properties": {}, "required": []})
    async def cancel_key_tool(args: dict) -> dict:
        if err := await _check_active("generate_key", tg_id):
            return _err(err)
        return _ok("已取消当前 Key。") if key.cancel() else _err("当前没有有效的 Key。")

    # ----- 用户列表 -----

    @tool("list_users", "列出用户（不含自己）。",
          {"type": "object", "properties": {}, "required": []})
    async def list_users(args: dict) -> dict:
        targets = await permission.get_manageable_targets(tg_id, "view_self_intro", is_superadmin)
        if targets == "_all":
            query_filter = {"tg_id": {"$ne": tg_id}}
        elif targets:
            query_filter = {"tg_id": {"$in": [t for t in targets if t != tg_id]}}
        else:
            return _ok("你没有查看任何用户的权限。")
        cursor = get_db().users.find(query_filter, {"_id": 0, "tg_id": 1, "name": 1, "nickname": 1, "position": 1})
        users = await cursor.to_list(length=100)
        if not users:
            return _ok("没有其他用户。")
        lines = [f"{u.get('name', '未设置')}({u['tg_id']}) - {u.get('position', '')}" for u in users]
        return _ok("\n".join(lines))

    # ----- 画像查看 -----

    @tool("view_user_infos", "查看某用户的画像。只返回你有权查看的部分。",
          {"type": "object", "properties": {"target_tg_id": {"type": "integer"}}, "required": ["target_tg_id"]})
    async def view_user_infos(args: dict) -> dict:
        target = args["target_tg_id"]
        all_infos = await profile.get_all(target)
        if not all_infos:
            return _ok("该用户暂无画像。")
        lines = []
        for author_id_str, items in all_infos.items():
            author_id = int(author_id_str)
            if await permission.can_view_infos(tg_id, target, author_id, is_superadmin):
                label = "自我介绍" if author_id == target else f"作者:{author_id_str}"
                for item in items:
                    lines.append(f"[{label}] {item}")
        return _ok("\n".join(lines)) if lines else _ok("你没有权限查看该用户的任何画像。")

    # ----- 画像编辑 -----

    @tool("get_my_infos_on_user", "查看我对某用户写的画像列表。",
          {"type": "object", "properties": {"target_tg_id": {"type": "integer"}}, "required": ["target_tg_id"]})
    async def get_my_infos_on_user(args: dict) -> dict:
        target = args["target_tg_id"]
        if err := await _check_active("write_profile", target):
            return _err(err)
        items = await profile.get_by_author(target, tg_id)
        return _ok(str(items)) if items else _ok("你还没给该用户写过画像。")

    @tool("save_infos_on_user", "保存我对某用户的画像列表（整体替换）。",
          {"type": "object", "properties": {
              "target_tg_id": {"type": "integer"},
              "items": {"type": "array", "items": {"type": "string"}},
          }, "required": ["target_tg_id", "items"]})
    async def save_infos_on_user(args: dict) -> dict:
        target = args["target_tg_id"]
        if not await _user_exists(target):
            return _err("目标用户不存在。")
        if err := await _check_active("write_profile", target):
            return _err(err)
        items = [s.strip() for s in args["items"] if isinstance(s, str) and s.strip()]
        if not items:
            return _err("列表为空，未保存。")
        await profile.replace(target, tg_id, items)
        return _ok("已保存。")

    # ----- 主动权限管理 -----

    @tool("grant_active_perm", "给某人授予主动权限。action: write_profile/view_self_intro/manage_perm/generate_key。target: tg_id 或 '_all'。",
          {"type": "object", "properties": {
              "subject_tg_id": {"type": "integer"},
              "action": {"type": "string"},
              "target": {"type": "string"},
          }, "required": ["subject_tg_id", "action", "target"]})
    async def grant_active_perm(args: dict) -> dict:
        subject, action = args["subject_tg_id"], args["action"]
        if not await _user_exists(subject):
            return _err("被授权用户不存在。")
        if err := await _check_active("manage_perm", subject):
            return _err(err)
        if action not in permission.ACTIVE_ACTIONS:
            return _err(f"无效的权限类型：{action}")
        try:
            target = _parse_target(args["target"])
        except ValueError as e:
            return _err(str(e))
        # 非超管只能转授自己拥有的权限，且范围不能超过自己
        if not is_superadmin:
            if target == "_all":
                # 要授 _all 范围，自己必须也有 _all
                own_targets = await permission.get_manageable_targets(tg_id, action, False)
                if own_targets != "_all":
                    return _err(f"你没有 {action} 的全局权限，无法授予 _all。")
            else:
                if not await permission.check_active(tg_id, action, target, False):
                    return _err(f"你自己没有对该目标的 {action} 权限，无法转授。")
        await permission.grant_active(subject, action, target)
        auth.invalidate(subject)
        return _ok("已授权。")

    @tool("revoke_active_perm", "撤销某人的主动权限。",
          {"type": "object", "properties": {
              "subject_tg_id": {"type": "integer"},
              "action": {"type": "string"},
              "target": {"type": "string"},
          }, "required": ["subject_tg_id", "action", "target"]})
    async def revoke_active_perm(args: dict) -> dict:
        subject = args["subject_tg_id"]
        if err := await _check_active("manage_perm", subject):
            return _err(err)
        try:
            target = _parse_target(args["target"])
        except ValueError as e:
            return _err(str(e))
        await permission.revoke_active(subject, args["action"], target)
        auth.invalidate(subject)
        return _ok("已撤销。")

    # ----- 被动权限管理 -----

    @tool("grant_passive_perm", "给某用户添加被动权限。需要 manage_perm 权限。",
          {"type": "object", "properties": {
              "target_tg_id": {"type": "integer"},
              "viewer": {"type": "string"},
              "action": {"type": "string"},
          }, "required": ["target_tg_id", "viewer", "action"]})
    async def grant_passive_perm(args: dict) -> dict:
        target = args["target_tg_id"]
        if err := await _check_active("manage_perm", target):
            return _err(err)
        if not permission.is_valid_passive_action(args["action"]):
            return _err("无效的权限格式。")
        try:
            viewer = _parse_target(args["viewer"])
        except ValueError as e:
            return _err(str(e))
        await permission.grant_passive(target, args["action"], viewer)
        return _ok("已授权。")

    @tool("revoke_passive_perm", "撤销某用户的被动权限。",
          {"type": "object", "properties": {
              "target_tg_id": {"type": "integer"},
              "viewer": {"type": "string"},
              "action": {"type": "string"},
          }, "required": ["target_tg_id", "viewer", "action"]})
    async def revoke_passive_perm(args: dict) -> dict:
        target = args["target_tg_id"]
        if err := await _check_active("manage_perm", target):
            return _err(err)
        try:
            viewer = _parse_target(args["viewer"])
        except ValueError as e:
            return _err(str(e))
        await permission.revoke_passive(target, args["action"], viewer)
        return _ok("已撤销。")

    # ----- 查看权限 -----

    @tool("view_user_perms", "查看某用户的所有权限。",
          {"type": "object", "properties": {"target_tg_id": {"type": "integer"}}, "required": ["target_tg_id"]})
    async def view_user_perms(args: dict) -> dict:
        target = args["target_tg_id"]
        if err := await _check_active("manage_perm", target):
            return _err(err)
        active = await permission.list_active(target)
        passive = await permission.list_passive(target)
        lines = []
        if active:
            lines.append("主动权限：")
            for k, v in active.items():
                lines.append(f"  对 {k}: {', '.join(v)}")
        if passive:
            lines.append("被动权限：")
            for k, v in passive.items():
                lines.append(f"  允许 {k}: {', '.join(v)}")
        return _ok("\n".join(lines)) if lines else _ok("该用户没有任何权限。")

    # ----- 项目管理 -----

    @tool("create_project", "创建一个新项目。需要 create_project 权限。",
          {"type": "object", "properties": {
              "name": {"type": "string"},
              "description": {"type": "string"},
          }, "required": ["name", "description"]})
    async def create_project_tool(args: dict) -> dict:
        # 超管或有 create_project 权限的人可以创建
        targets = await permission.get_manageable_targets(tg_id, "create_project", is_superadmin)
        if isinstance(targets, list) and not targets:
            return _err("你没有创建项目的权限。")
        pid = await task.create_project(args["name"], args["description"], tg_id)
        return _ok(f"项目已创建：{args['name']} (id:{pid})")

    @tool("list_my_projects", "查看我创建的项目。",
          {"type": "object", "properties": {}, "required": []})
    async def list_my_projects(args: dict) -> dict:
        projects = await task.list_projects_by_creator(tg_id)
        if not projects:
            return _ok("你没有活跃的项目。")
        lines = [f"{p['name']} (id:{p['_id']})" for p in projects]
        return _ok("\n".join(lines))

    @tool("assign_task", "在项目中创建任务并分配给某人。需要对该人有 create_project 权限。",
          {"type": "object", "properties": {
              "project_id": {"type": "string"},
              "title": {"type": "string"},
              "goal": {"type": "string", "description": "任务目标（为什么做）"},
              "description": {"type": "string", "description": "具体描述（做什么）"},
              "assignee_tg_id": {"type": "integer"},
          }, "required": ["project_id", "title", "goal", "description", "assignee_tg_id"]})
    async def assign_task_tool(args: dict) -> dict:
        # 校验项目存在且是自己创建的（或超管）
        project = await task.get_project(args["project_id"])
        if not project:
            return _err("项目不存在。")
        if project["creator_tg_id"] != tg_id and not is_superadmin:
            return _err("你不是该项目的创建者。")
        # 校验对被分配人有 create_project 权限
        assignee = args["assignee_tg_id"]
        if not await permission.check_active(tg_id, "create_project", assignee, is_superadmin):
            return _err(f"你没有把任务分配给 {assignee} 的权限。")
        if not await _user_exists(assignee):
            return _err(f"用户 {assignee} 不存在。")
        tid = await task.create_task(
            project_id=args["project_id"],
            title=args["title"], goal=args["goal"],
            description=args["description"],
            assignee_tg_id=assignee, assigner_tg_id=tg_id,
        )
        await permission.inherit_view_perms(tg_id, assignee, is_superadmin)
        auth.invalidate(assignee)
        await notify.task_assigned(assignee, args["title"], args["goal"], tg_id)
        return _ok(f"任务已创建并分配给 {assignee} (id:{tid})")

    @tool("view_project", "查看项目的完整任务树（含拆分详情）。需要是项目创建者。",
          {"type": "object", "properties": {
              "project_id": {"type": "string"},
          }, "required": ["project_id"]})
    async def view_project_tool(args: dict) -> dict:
        project = await task.get_project(args["project_id"])
        if not project:
            return _err("项目不存在。")
        if project["creator_tg_id"] != tg_id and not is_superadmin:
            return _err("你不是该项目的创建者。")
        # 找顶级任务（由项目创建者直接分配的）
        all_tasks = await task.get_project_tasks(args["project_id"])
        if not all_tasks:
            return _ok("项目下没有任务。")
        top_ids = [str(t["_id"]) for t in all_tasks if t["assigner_tg_id"] == project["creator_tg_id"]]
        if not top_ids:
            return _ok("项目下没有顶级任务。")

        output = [f"项目：{project['name']}"]
        for tid in top_ids:
            tree = await task.get_task_tree(tid)
            if tree:
                output.extend(_fmt_tree(tree))
        return _ok("\n".join(output))

    @tool("archive_project", "归档项目。",
          {"type": "object", "properties": {"project_id": {"type": "string"}}, "required": ["project_id"]})
    async def archive_project_tool(args: dict) -> dict:
        project = await task.get_project(args["project_id"])
        if not project:
            return _err("项目不存在。")
        if project["creator_tg_id"] != tg_id and not is_superadmin:
            return _err("你不是该项目的创建者。")
        ok = await task.archive_project(args["project_id"])
        return _ok("项目已归档。") if ok else _err("归档失败。")

    @tool("delete_project", "删除项目及其所有任务。不可恢复。",
          {"type": "object", "properties": {"project_id": {"type": "string"}}, "required": ["project_id"]})
    async def delete_project_tool(args: dict) -> dict:
        project = await task.get_project(args["project_id"])
        if not project:
            return _err("项目不存在。")
        if project["creator_tg_id"] != tg_id and not is_superadmin:
            return _err("你不是该项目的创建者。")
        await task.delete_project(args["project_id"])
        return _ok("项目及所有任务已删除。")

    # ----- 用户信息管理 -----

    @tool("update_user_info", "修改某用户的基本信息。需要 edit_info 主动权限。",
          {"type": "object", "properties": {
              "target_tg_id": {"type": "integer"},
              "fields_data": {"type": "object", "description": "要更新的字段"},
          }, "required": ["target_tg_id", "fields_data"]})
    async def update_user_info(args: dict) -> dict:
        target = args["target_tg_id"]
        if err := await _check_active("edit_info", target):
            return _err(err)
        if not await _user_exists(target):
            return _err("用户不存在。")
        from datetime import datetime, timezone
        valid_fields = set(fields.get_fields())
        update_data = {k: v for k, v in args["fields_data"].items() if k in valid_fields and isinstance(v, str)}
        if not update_data:
            return _err("没有可更新的有效字段。")
        update_data["updated_at"] = datetime.now(timezone.utc)
        await get_db().users.update_one({"tg_id": target}, {"$set": update_data})
        auth.invalidate(target)
        return _ok("已更新。")

    @tool("get_user_info", "查看某用户的基本信息。需要 view_self_intro 主动权限。",
          {"type": "object", "properties": {"target_tg_id": {"type": "integer"}}, "required": ["target_tg_id"]})
    async def get_user_info(args: dict) -> dict:
        target = args["target_tg_id"]
        if err := await _check_active("view_self_intro", target):
            return _err(err)
        doc = await get_db().users.find_one({"tg_id": target}, _profile_projection())
        if not doc:
            return _err("用户不存在。")
        return _ok(str(doc))

    # ----- 字段管理 -----

    @tool("add_info_field", "添加一个基本信息字段。超管专用。",
          {"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]})
    async def add_info_field(args: dict) -> dict:
        if not is_superadmin:
            return _err("只有超管可以管理字段。")
        ok = await fields.add_field(args["name"])
        return _ok(f"字段 '{args['name']}' 已添加。") if ok else _err("字段已存在或无效。")

    @tool("remove_info_field", "移除一个基本信息字段。超管专用。",
          {"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]})
    async def remove_info_field(args: dict) -> dict:
        if not is_superadmin:
            return _err("只有超管可以管理字段。")
        ok = await fields.remove_field(args["name"])
        return _ok(f"字段 '{args['name']}' 已移除。") if ok else _err("字段不存在。")

    @tool("list_info_fields", "查看当前所有基本信息字段。",
          {"type": "object", "properties": {}, "required": []})
    async def list_info_fields(args: dict) -> dict:
        return _ok(", ".join(fields.get_fields()))

    # ----- 用户管理 -----

    @tool("disable_user", "禁用一个用户（从系统中移除，清除权限）。超管专用。",
          {"type": "object", "properties": {"target_tg_id": {"type": "integer"}}, "required": ["target_tg_id"]})
    async def disable_user(args: dict) -> dict:
        if not is_superadmin:
            return _err("只有超管可以禁用用户。")
        target = args["target_tg_id"]
        if not await _user_exists(target):
            return _err("用户不存在。")
        await get_db().users.delete_one({"tg_id": target})
        auth.invalidate(target)
        return _ok(f"用户 {target} 已禁用。")

    # ----- 角色管理 -----

    @tool("create_role", "创建一个角色/Skill。超管专用。",
          {"type": "object", "properties": {
              "name": {"type": "string", "description": "角色名称"},
              "trigger": {"type": "string", "description": "触发场景描述"},
              "prompt": {"type": "string", "description": "角色的完整提示词"},
          }, "required": ["name", "trigger", "prompt"]})
    async def create_role(args: dict) -> dict:
        if not is_superadmin:
            return _err("只有超管可以创建角色。")
        if role.get_by_name(args["name"]):
            return _err(f"角色 '{args['name']}' 已存在。")
        await role.create(args["name"], args["trigger"], args["prompt"], tg_id)
        return _ok(f"角色已创建：{args['name']}")

    @tool("update_role", "更新角色的触发描述或提示词。超管专用。",
          {"type": "object", "properties": {
              "name": {"type": "string"},
              "trigger": {"type": "string"},
              "prompt": {"type": "string"},
          }, "required": ["name"]})
    async def update_role(args: dict) -> dict:
        if not is_superadmin:
            return _err("只有超管可以更新角色。")
        ok = await role.update(args["name"], args.get("trigger"), args.get("prompt"))
        return _ok("角色已更新。") if ok else _err("角色不存在。")

    @tool("delete_role", "删除一个角色。超管专用。",
          {"type": "object", "properties": {"name": {"type": "string"}}, "required": ["name"]})
    async def delete_role(args: dict) -> dict:
        if not is_superadmin:
            return _err("只有超管可以删除角色。")
        ok = await role.delete(args["name"])
        return _ok("角色已删除。") if ok else _err("角色不存在。")

    @tool("list_roles", "查看所有角色。",
          {"type": "object", "properties": {}, "required": []})
    async def list_roles(args: dict) -> dict:
        roles = await role.list_all()
        if not roles:
            return _ok("暂无角色。")
        lines = [f"{r['name']}：{r['trigger']}" for r in roles]
        return _ok("\n".join(lines))

    _admin_tool_list = [
        generate_key_tool, cancel_key_tool, list_users,
        view_user_infos, get_my_infos_on_user, save_infos_on_user,
        grant_active_perm, revoke_active_perm,
        grant_passive_perm, revoke_passive_perm,
        view_user_perms,
        create_project_tool, list_my_projects, assign_task_tool,
        view_project_tool, archive_project_tool, delete_project_tool,
        update_user_info, get_user_info,
        add_info_field, remove_info_field, list_info_fields,
        disable_user,
        create_role, update_role, delete_role, list_roles,
    ]
    return create_sdk_mcp_server(name="admin", version="1.0.0", tools=_admin_tool_list), _admin_tool_list


# ===== Telegram Tools =====

def create_telegram_tools(bot, tg_id: int, is_superadmin: bool, own_chat_id: int):
    """绑 bot + 用户身份 + 当前 chat_id"""

    @tool("send_message", "向指定用户发送消息。超管可发给任何人，其他人需要 send_msg 主动权限。",
          {"type": "object", "properties": {
              "chat_id": {"type": "integer", "description": "目标 chat_id"},
              "text": {"type": "string", "description": "消息内容，支持 HTML"},
          }, "required": ["chat_id", "text"]})
    async def send_message(args: dict) -> dict:
        target_chat = args["chat_id"]
        # 给自己的对话发不需要权限
        if target_chat != own_chat_id:
            if not await permission.check_active(tg_id, "send_msg", target_chat, is_superadmin):
                return _err("你没有权限向该用户发送消息。")
        try:
            msg = await bot.send_message(chat_id=target_chat, text=args["text"], parse_mode="HTML")
            return _ok(f"已发送，message_id={msg.message_id}")
        except Exception as e:
            return _err(f"发送失败：{e}")

    @tool("edit_message", "编辑当前对话中已发送的消息。",
          {"type": "object", "properties": {
              "message_id": {"type": "integer"}, "text": {"type": "string"},
          }, "required": ["message_id", "text"]})
    async def edit_message(args: dict) -> dict:
        try:
            await bot.edit_message_text(chat_id=own_chat_id, message_id=args["message_id"], text=args["text"], parse_mode="HTML")
            return _ok("已编辑。")
        except Exception as e:
            return _err(f"编辑失败：{e}")

    @tool("delete_message", "删除当前对话中的一条消息。",
          {"type": "object", "properties": {
              "message_id": {"type": "integer"},
          }, "required": ["message_id"]})
    async def delete_message(args: dict) -> dict:
        try:
            await bot.delete_message(chat_id=own_chat_id, message_id=args["message_id"])
            return _ok("已删除。")
        except Exception as e:
            return _err(f"删除失败：{e}")

    @tool("send_photo", "发送图片。可附带说明文字。",
          {"type": "object", "properties": {
              "chat_id": {"type": "integer"},
              "file_id": {"type": "string", "description": "Telegram file_id"},
              "caption": {"type": "string", "description": "图片说明（可选）"},
          }, "required": ["chat_id", "file_id"]})
    async def send_photo(args: dict) -> dict:
        target_chat = args["chat_id"]
        if target_chat != own_chat_id:
            if not await permission.check_active(tg_id, "send_msg", target_chat, is_superadmin):
                return _err("你没有权限向该用户发送消息。")
        try:
            msg = await bot.send_photo(
                chat_id=target_chat, photo=args["file_id"],
                caption=args.get("caption", ""), parse_mode="HTML",
            )
            return _ok(f"已发送图片，message_id={msg.message_id}")
        except Exception as e:
            return _err(f"发送失败：{e}")

    @tool("send_local_photo", "发送本地图片文件（如 AI 生成的图片）。",
          {"type": "object", "properties": {
              "chat_id": {"type": "integer"},
              "file_path": {"type": "string", "description": "本地图片文件路径"},
              "caption": {"type": "string", "description": "图片说明（可选）"},
          }, "required": ["chat_id", "file_path"]})
    async def send_local_photo(args: dict) -> dict:
        import os
        target_chat = args["chat_id"]
        if target_chat != own_chat_id:
            if not await permission.check_active(tg_id, "send_msg", target_chat, is_superadmin):
                return _err("你没有权限向该用户发送消息。")
        path = os.path.realpath(args["file_path"])
        # 限制只能发送工作目录下的图片
        allowed_dir = os.path.realpath(os.getcwd())
        if not path.startswith(allowed_dir):
            return _err("只能发送工作目录下的图片。")
        if not os.path.isfile(path):
            return _err(f"文件不存在：{path}")
        try:
            with open(path, "rb") as f:
                msg = await bot.send_photo(
                    chat_id=target_chat, photo=f,
                    caption=args.get("caption", ""), parse_mode="HTML",
                )
            return _ok(f"已发送图片，message_id={msg.message_id}")
        except Exception as e:
            return _err(f"发送失败：{e}")

    return create_sdk_mcp_server(name="telegram", version="1.0.0", tools=[
        send_message, edit_message, delete_message, send_photo, send_local_photo,
    ])
