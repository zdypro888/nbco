"""任务模块

tasks 集合结构:
{
    _id: ObjectId,
    project_id: ObjectId,
    title: str,
    goal: str,                      # 任务目标（为什么做），只有 assigner 能改
    description: str,               # 具体描述（做什么），只有 assigner 能改
    assignee_tg_id: int,
    assigner_tg_id: int,
    status: "pending" | "in_progress" | "done" | "cancelled" | "split",
    checklist: [                     # 工作清单，assignee 管理
        { "item": str, "done": bool }
    ],
    progress_notes: [                # 进度日志，assignee 追加
        { "content": str, "time": datetime }
    ],
    split_into: [                    # 拆分记录
        { "task_id": ObjectId, "assignee_tg_id": int }
    ] | null,
    split_at: datetime | null,
    created_at: datetime,
    updated_at: datetime,
}
"""

from datetime import datetime, timezone
from bson import ObjectId
from db import get_db

UPDATABLE_STATUSES = {"pending", "in_progress", "done", "cancelled"}
_NOW = lambda: datetime.now(timezone.utc)


# ===== 项目 =====

async def create_project(name: str, description: str, creator_tg_id: int) -> str:
    now = _NOW()
    result = await get_db().projects.insert_one({
        "name": name, "description": description,
        "creator_tg_id": creator_tg_id, "status": "active",
        "created_at": now, "updated_at": now,
    })
    return str(result.inserted_id)


async def get_project(project_id: str) -> dict | None:
    try:
        return await get_db().projects.find_one({"_id": ObjectId(project_id)})
    except Exception:
        return None


async def list_projects_by_creator(creator_tg_id: int) -> list[dict]:
    cursor = get_db().projects.find(
        {"creator_tg_id": creator_tg_id, "status": "active"},
        {"_id": 1, "name": 1, "description": 1, "created_at": 1},
    )
    return await cursor.to_list(length=50)


async def archive_project(project_id: str) -> bool:
    try:
        result = await get_db().projects.update_one(
            {"_id": ObjectId(project_id)},
            {"$set": {"status": "archived", "updated_at": _NOW()}},
        )
        return result.modified_count > 0
    except Exception:
        return False


# ===== 任务 CRUD =====

async def create_task(
    project_id: str, title: str, goal: str, description: str,
    assignee_tg_id: int, assigner_tg_id: int,
) -> str:
    now = _NOW()
    result = await get_db().tasks.insert_one({
        "project_id": ObjectId(project_id),
        "title": title, "goal": goal, "description": description,
        "assignee_tg_id": assignee_tg_id, "assigner_tg_id": assigner_tg_id,
        "status": "pending",
        "checklist": [], "progress_notes": [],
        "split_into": None, "split_at": None,
        "created_at": now, "updated_at": now,
    })
    return str(result.inserted_id)


async def get_task(task_id: str) -> dict | None:
    try:
        return await get_db().tasks.find_one({"_id": ObjectId(task_id)})
    except Exception:
        return None


# ===== 状态管理 =====

async def update_status(task_id: str, status: str) -> bool:
    if status not in UPDATABLE_STATUSES:
        return False
    try:
        result = await get_db().tasks.update_one(
            {"_id": ObjectId(task_id), "status": {"$ne": "split"}},
            {"$set": {"status": status, "updated_at": _NOW()}},
        )
        return result.modified_count > 0
    except Exception:
        return False


async def mark_split(task_id: str, children: list[dict]) -> bool:
    """children: [{"task_id": str, "assignee_tg_id": int}]"""
    now = _NOW()
    records = [{"task_id": ObjectId(c["task_id"]), "assignee_tg_id": c["assignee_tg_id"]} for c in children]
    try:
        result = await get_db().tasks.update_one(
            {"_id": ObjectId(task_id), "status": {"$nin": ["split", "done", "cancelled"]}},
            {"$set": {"status": "split", "split_into": records, "split_at": now, "updated_at": now}},
        )
        return result.modified_count > 0
    except Exception:
        return False


# ===== Description（只有 assigner 能改） =====

async def update_task_fields(task_id: str, goal: str | None = None, description: str | None = None) -> bool:
    """更新 goal 和/或 description，只有 assigner 能调"""
    fields = {"updated_at": _NOW()}
    if goal is not None:
        fields["goal"] = goal
    if description is not None:
        fields["description"] = description
    try:
        result = await get_db().tasks.update_one({"_id": ObjectId(task_id)}, {"$set": fields})
        return result.modified_count > 0
    except Exception:
        return False


# ===== Checklist（assignee 管理） =====

async def set_checklist(task_id: str, items: list[dict]) -> bool:
    """整体替换 checklist。items: [{"item": str, "done": bool}]"""
    try:
        result = await get_db().tasks.update_one(
            {"_id": ObjectId(task_id)},
            {"$set": {"checklist": items, "updated_at": _NOW()}},
        )
        return result.modified_count > 0
    except Exception:
        return False


async def toggle_checklist_item(task_id: str, index: int, done: bool) -> bool:
    try:
        result = await get_db().tasks.update_one(
            {"_id": ObjectId(task_id), f"checklist.{index}": {"$exists": True}},
            {"$set": {f"checklist.{index}.done": done, "updated_at": _NOW()}},
        )
        return result.modified_count > 0
    except Exception:
        return False


# ===== Progress Notes（assignee 追加） =====

async def add_progress_note(task_id: str, content: str) -> bool:
    try:
        result = await get_db().tasks.update_one(
            {"_id": ObjectId(task_id)},
            {"$push": {"progress_notes": {"content": content, "time": _NOW()}}, "$set": {"updated_at": _NOW()}},
        )
        return result.modified_count > 0
    except Exception:
        return False


# ===== 查询 =====

async def delete_task(task_id: str) -> bool:
    """删除任务及其所有子任务（递归）"""
    t = await get_task(task_id)
    if not t:
        return False
    # 先递归删除子任务
    if t.get("split_into"):
        for child in t["split_into"]:
            await delete_task(str(child["task_id"]))
    try:
        result = await get_db().tasks.delete_one({"_id": ObjectId(task_id)})
        return result.deleted_count > 0
    except Exception:
        return False


async def delete_project_tasks(project_id: str) -> int:
    """删除项目下所有任务，返回删除数量"""
    try:
        result = await get_db().tasks.delete_many({"project_id": ObjectId(project_id)})
        return result.deleted_count
    except Exception:
        return 0


async def delete_project(project_id: str) -> bool:
    """删除项目及其所有任务"""
    await delete_project_tasks(project_id)
    try:
        result = await get_db().projects.delete_one({"_id": ObjectId(project_id)})
        return result.deleted_count > 0
    except Exception:
        return False


async def get_my_tasks(tg_id: int) -> list[dict]:
    cursor = get_db().tasks.find(
        {"assignee_tg_id": tg_id, "status": {"$in": ["pending", "in_progress"]}},
    )
    return await cursor.to_list(length=100)


async def get_my_all_tasks(tg_id: int) -> list[dict]:
    cursor = get_db().tasks.find({"assignee_tg_id": tg_id})
    return await cursor.to_list(length=200)


async def get_assigned_by_me(tg_id: int) -> list[dict]:
    """查看我分配出去的任务"""
    cursor = get_db().tasks.find(
        {"assigner_tg_id": tg_id, "assignee_tg_id": {"$ne": tg_id}},
        {"_id": 1, "title": 1, "assignee_tg_id": 1, "status": 1, "checklist": 1, "progress_notes": 1},
    )
    return await cursor.to_list(length=200)


async def get_project_tasks(project_id: str) -> list[dict]:
    try:
        cursor = get_db().tasks.find({"project_id": ObjectId(project_id)})
        return await cursor.to_list(length=200)
    except Exception:
        return []


async def get_task_tree(task_id: str) -> dict | None:
    """递归获取任务及其拆分子树"""
    t = await get_task(task_id)
    if not t:
        return None
    total = len(t.get("checklist", []))
    done = sum(1 for c in t.get("checklist", []) if c.get("done"))
    result = {
        "task_id": str(t["_id"]),
        "title": t["title"],
        "goal": t.get("goal", ""),
        "assignee_tg_id": t["assignee_tg_id"],
        "status": t["status"],
        "progress": f"{done}/{total}" if total else "无清单",
        "children": [],
    }
    if t.get("split_into"):
        for child in t["split_into"]:
            child_tree = await get_task_tree(str(child["task_id"]))
            if child_tree:
                result["children"].append(child_tree)
    return result
