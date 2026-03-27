"""定时任务模块，基于 python-telegram-bot JobQueue + MongoDB 持久化"""

import logging
from datetime import datetime, timezone
from db import get_db
import notify

logger = logging.getLogger(__name__)

_app = None
_known_users: set[int] = set()
_superadmins: list[int] = []
_custom_jobs: dict[str, object] = {}
_MAX_JOBS_PER_USER = 10


def init(app, superadmins: list[int]):
    global _app, _superadmins
    _app = app
    _superadmins = superadmins
    jq = app.job_queue
    jq.run_once(_load_known_users, when=0)
    jq.run_repeating(_check_new_users, interval=300, first=10)
    jq.run_daily(_daily_task_summary, time=_make_time(1, 0))
    jq.run_once(_restore_jobs, when=1)
    logger.info("定时任务已启动")


def _make_time(hour, minute):
    from datetime import time as dt_time
    return dt_time(hour=hour, minute=minute, tzinfo=timezone.utc)


def _job_key(tg_id: int, name: str) -> str:
    return f"{tg_id}:{name}"


def _count_user_jobs(tg_id: int) -> int:
    return sum(1 for k in _custom_jobs if k.startswith(f"{tg_id}:"))


# ===== 内置定时任务 =====

async def _load_known_users(context):
    global _known_users
    cursor = get_db().users.find({}, {"tg_id": 1})
    docs = await cursor.to_list(length=1000)
    _known_users = {doc["tg_id"] for doc in docs}
    logger.info(f"已加载 {len(_known_users)} 个已知用户")


async def _check_new_users(context):
    global _known_users
    cursor = get_db().users.find({}, {"tg_id": 1, "name": 1})
    docs = await cursor.to_list(length=1000)
    current_ids = {doc["tg_id"] for doc in docs}
    new_ids = current_ids - _known_users
    if new_ids:
        for doc in docs:
            if doc["tg_id"] in new_ids:
                name = doc.get("name", "未设置")
                for admin_id in _superadmins:
                    await notify.send(admin_id, f"👤 新员工加入：{name} ({doc['tg_id']})")
        _known_users = current_ids


async def _daily_task_summary(context):
    cursor = get_db().tasks.aggregate([
        {"$match": {"status": {"$in": ["pending", "in_progress"]}}},
        {"$group": {"_id": "$assignee_tg_id", "count": {"$sum": 1}}},
    ])
    async for group in cursor:
        await notify.send(group["_id"], f"📋 你有 {group['count']} 个待办任务，记得更新进度。")


# ===== 自定义定时任务回调 =====

async def _custom_job_callback(context):
    data = context.job.data
    await notify.send(data["tg_id"], data["message"])
    key = context.job.name
    if not data.get("repeating"):
        _custom_jobs.pop(key, None)
        await get_db().schedules.delete_one({"key": key})


# ===== 持久化恢复 =====

async def _restore_jobs(context):
    cursor = get_db().schedules.find({})
    docs = await cursor.to_list(length=500)
    now = datetime.now(timezone.utc)
    restored = 0
    for doc in docs:
        key = doc["key"]
        if key in _custom_jobs:
            continue
        if doc["repeating"]:
            job = _app.job_queue.run_repeating(
                _custom_job_callback, interval=doc["interval"], first=doc["interval"],
                data={"tg_id": doc["tg_id"], "message": doc["message"], "repeating": True}, name=key,
            )
            _custom_jobs[key] = job
            restored += 1
        else:
            run_at = doc.get("run_at")
            if run_at and run_at > now:
                delay = (run_at - now).total_seconds()
                job = _app.job_queue.run_once(
                    _custom_job_callback, when=delay,
                    data={"tg_id": doc["tg_id"], "message": doc["message"], "repeating": False}, name=key,
                )
                _custom_jobs[key] = job
                restored += 1
            else:
                await get_db().schedules.delete_one({"key": key})
    if restored:
        logger.info(f"已恢复 {restored} 个定时任务")


# ===== 用户操作（async，从 tool 调用） =====

async def schedule_once(tg_id: int, name: str, message: str, run_at: datetime, is_superadmin: bool = False) -> str | None:
    if not _app:
        return "调度器未初始化。"
    key = _job_key(tg_id, name)
    if key in _custom_jobs:
        return "名称已存在。"
    if not is_superadmin and _count_user_jobs(tg_id) >= _MAX_JOBS_PER_USER:
        return f"最多 {_MAX_JOBS_PER_USER} 个定时任务。"
    delay = (run_at - datetime.now(timezone.utc)).total_seconds()
    if delay < 0:
        return "时间已过。"
    job = _app.job_queue.run_once(
        _custom_job_callback, when=delay,
        data={"tg_id": tg_id, "message": message, "repeating": False}, name=key,
    )
    _custom_jobs[key] = job
    await get_db().schedules.update_one(
        {"key": key},
        {"$set": {"key": key, "tg_id": tg_id, "message": message, "repeating": False, "run_at": run_at}},
        upsert=True,
    )
    return None


async def schedule_repeating(tg_id: int, name: str, message: str, interval_seconds: int, is_superadmin: bool = False) -> str | None:
    if not _app:
        return "调度器未初始化。"
    key = _job_key(tg_id, name)
    if key in _custom_jobs:
        return "名称已存在。"
    if not is_superadmin and _count_user_jobs(tg_id) >= _MAX_JOBS_PER_USER:
        return f"最多 {_MAX_JOBS_PER_USER} 个定时任务。"
    if interval_seconds < 60:
        return "最小间隔 60 秒。"
    job = _app.job_queue.run_repeating(
        _custom_job_callback, interval=interval_seconds, first=interval_seconds,
        data={"tg_id": tg_id, "message": message, "repeating": True}, name=key,
    )
    _custom_jobs[key] = job
    await get_db().schedules.update_one(
        {"key": key},
        {"$set": {"key": key, "tg_id": tg_id, "message": message, "repeating": True, "interval": interval_seconds}},
        upsert=True,
    )
    return None


async def cancel_job(tg_id: int, name: str) -> bool:
    key = _job_key(tg_id, name)
    if key not in _custom_jobs:
        return False
    _custom_jobs[key].schedule_removal()
    del _custom_jobs[key]
    await get_db().schedules.delete_one({"key": key})
    return True


def list_user_jobs(tg_id: int) -> list[dict]:
    prefix = f"{tg_id}:"
    result = []
    for key, job in _custom_jobs.items():
        if key.startswith(prefix):
            result.append({
                "name": key[len(prefix):],
                "message": job.data["message"],
                "repeating": job.data.get("repeating", False),
                "next_run": str(job.next_t) if job.next_t else "已完成",
            })
    return result
