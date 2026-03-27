import secrets
from datetime import datetime, timezone
from db import get_db
import auth

# 当前有效的 Key，None 表示没有
_current_key: str | None = None


def generate() -> str:
    """生成绑定 Key，替换之前的"""
    global _current_key
    _current_key = secrets.token_hex(8)
    return _current_key


def cancel() -> bool:
    """取消当前 Key"""
    global _current_key
    if _current_key is None:
        return False
    _current_key = None
    return True


async def consume(code: str, tg_id: int) -> bool:
    """使用 Key 绑定用户"""
    if _current_key is None or code != _current_key:
        return False

    await get_db().users.update_one(
        {"tg_id": tg_id},
        {"$setOnInsert": {"tg_id": tg_id, "created_at": datetime.now(timezone.utc)}},
        upsert=True,
    )
    auth.invalidate(tg_id)
    return True
