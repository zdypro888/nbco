"""定时任务模块，基于 asyncio + MongoDB 持久化，不依赖 TG 库"""

import asyncio
import logging
from datetime import datetime, timezone
from db import get_db
import notify

logger = logging.getLogger(__name__)

_superadmins: list[int] = []
_known_users: set[int] = set()
_custom_jobs: dict[str, asyncio.Task] = {}
_running = False
_MAX_JOBS_PER_USER = 10


async def init(superadmins: list[int]):
    global _superadmins, _running
    _superadmins = superadmins
    _running = True
    await _load_known_users()
    asyncio.create_task(_loop_check_new_users())
    asyncio.create_task(_loop_daily_summary())
    await _restore_jobs()
    logger.info("定时任务已启动")


async def shutdown():
    global _running
    _running = False
    for t in _custom_jobs.values():
        t.cancel()
    _custom_jobs.clear()


# ===== 内置循环任务 =====

async def _load_known_users():
    cursor = get_db().users.find({}, {"tg_id": 1})
    docs = await cursor.to_list(length=1000)
    _known_users.update(doc["tg_id"] for doc in docs)
    logger.info(f"已加载 {len(_known_users)} 个已知用户")


async def _loop_check_new_users():
    while _running:
        await asyncio.sleep(300)
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
            _known_users.update(new_ids)


async def _loop_daily_summary():
    while _running:
        now = datetime.now(timezone.utc)
        # 下次执行：明天 UTC 1:00（北京 9:00）
        tomorrow = now.replace(hour=1, minute=0, second=0, microsecond=0)
        if tomorrow <= now:
            tomorrow = tomorrow.replace(day=tomorrow.day + 1)
        await asyncio.sleep((tomorrow - now).total_seconds())
        if not _running:
            break
        cursor = get_db().tasks.aggregate([
            {"$match": {"status": {"$in": ["pending", "in_progress"]}}},
            {"$group": {"_id": "$assignee_tg_id", "count": {"$sum": 1}}},
        ])
        async for group in cursor:
            await notify.send(group["_id"], f"📋 你有 {group['count']} 个待办任务，记得更新进度。")


# ===== 自定义定时任务 =====

def _job_key(tg_id: int, name: str) -> str:
    return f"{tg_id}:{name}"


def _count_user_jobs(tg_id: int) -> int:
    return sum(1 for k in _custom_jobs if k.startswith(f"{tg_id}:"))


async def _run_once_job(key: str, tg_id: int, message: str, delay: float):
    await asyncio.sleep(delay)
    await notify.send(tg_id, message)
    _custom_jobs.pop(key, None)
    await get_db().schedules.delete_one({"key": key})


async def _run_repeating_job(key: str, tg_id: int, message: str, interval: int):
    while _running and key in _custom_jobs:
        await asyncio.sleep(interval)
        if key in _custom_jobs:
            await notify.send(tg_id, message)


async def _restore_jobs():
    cursor = get_db().schedules.find({})
    docs = await cursor.to_list(length=500)
    now = datetime.now(timezone.utc)
    restored = 0
    for doc in docs:
        key = doc["key"]
        if key in _custom_jobs:
            continue
        if doc["repeating"]:
            t = asyncio.create_task(_run_repeating_job(key, doc["tg_id"], doc["message"], doc["interval"]))
            _custom_jobs[key] = t
            restored += 1
        else:
            run_at = doc.get("run_at")
            if run_at:
                # MongoDB 存的可能是 naive datetime，统一转 UTC
                if run_at.tzinfo is None:
                    run_at = run_at.replace(tzinfo=timezone.utc)
            if run_at and run_at > now:
                delay = (run_at - now).total_seconds()
                t = asyncio.create_task(_run_once_job(key, doc["tg_id"], doc["message"], delay))
                _custom_jobs[key] = t
                restored += 1
            else:
                await get_db().schedules.delete_one({"key": key})
    if restored:
        logger.info(f"已恢复 {restored} 个定时任务")


# ===== 用户操作 =====

async def schedule_once(tg_id: int, name: str, message: str, run_at: datetime, is_superadmin: bool = False) -> str | None:
    key = _job_key(tg_id, name)
    if key in _custom_jobs:
        return "名称已存在。"
    if not is_superadmin and _count_user_jobs(tg_id) >= _MAX_JOBS_PER_USER:
        return f"最多 {_MAX_JOBS_PER_USER} 个定时任务。"
    delay = (run_at - datetime.now(timezone.utc)).total_seconds()
    if delay < 0:
        return "时间已过。"
    t = asyncio.create_task(_run_once_job(key, tg_id, message, delay))
    _custom_jobs[key] = t
    await get_db().schedules.update_one(
        {"key": key},
        {"$set": {"key": key, "tg_id": tg_id, "message": message, "repeating": False, "run_at": run_at}},
        upsert=True,
    )
    return None


async def schedule_repeating(tg_id: int, name: str, message: str, interval_seconds: int, is_superadmin: bool = False) -> str | None:
    key = _job_key(tg_id, name)
    if key in _custom_jobs:
        return "名称已存在。"
    if not is_superadmin and _count_user_jobs(tg_id) >= _MAX_JOBS_PER_USER:
        return f"最多 {_MAX_JOBS_PER_USER} 个定时任务。"
    if interval_seconds < 60:
        return "最小间隔 60 秒。"
    t = asyncio.create_task(_run_repeating_job(key, tg_id, message, interval_seconds))
    _custom_jobs[key] = t
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
    _custom_jobs[key].cancel()
    del _custom_jobs[key]
    await get_db().schedules.delete_one({"key": key})
    return True


def list_user_jobs(tg_id: int) -> list[dict]:
    prefix = f"{tg_id}:"
    result = []
    for key in _custom_jobs:
        if key.startswith(prefix):
            result.append({"name": key[len(prefix):]})
    return result
