import time
import logging
from user import User
from db import get_db

logger = logging.getLogger(__name__)

# 缓存: tg_id -> (User, expire_time)
_cache: dict[int, tuple[User, float]] = {}
_CACHE_TTL = 300

_superadmins: set[int] = set()


def init(superadmins: list[int]):
    global _superadmins
    _superadmins = set(superadmins)


async def _load_superadmins():
    for tg_id in _superadmins:
        doc = await get_db().users.find_one({"tg_id": tg_id})
        user = User(tg_id=tg_id, is_superadmin=True, has_profile=doc is not None, has_admin=True)
        _cache[tg_id] = (user, float("inf"))
        logger.info(f"超管已加载: {tg_id}, has_profile={user.has_profile}")


async def get_user(tg_id: int) -> User | None:
    now = time.time()
    if tg_id in _cache:
        user, expire = _cache[tg_id]
        if now < expire:
            return user

    doc = await get_db().users.find_one({"tg_id": tg_id})
    if doc is None:
        if tg_id in _superadmins:
            user = User(tg_id=tg_id, is_superadmin=True, has_profile=False, has_admin=True)
            _cache[tg_id] = (user, float("inf"))
            return user
        return None

    is_admin = tg_id in _superadmins
    has_perms = bool(doc.get("active_perms"))
    user = User(tg_id=doc["tg_id"], is_superadmin=is_admin, has_profile=True, has_admin=is_admin or has_perms)
    ttl = float("inf") if is_admin else now + _CACHE_TTL
    _cache[tg_id] = (user, ttl)
    return user


async def is_authorized(tg_id: int) -> bool:
    return await get_user(tg_id) is not None


def invalidate(tg_id: int):
    """清除缓存，下次访问重新从库加载"""
    _cache.pop(tg_id, None)
