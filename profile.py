"""画像/描述模块，user tools 和 admin tools 共用"""

from db import get_db


async def get_by_author(target_tg_id: int, author_tg_id: int) -> list[str]:
    """获取 target 上某个 author 写的画像"""
    author_key = str(author_tg_id)
    doc = await get_db().users.find_one(
        {"tg_id": target_tg_id},
        {f"infos.{author_key}": 1},
    )
    if not doc or "infos" not in doc:
        return []
    return doc["infos"].get(author_key, [])


async def get_all(target_tg_id: int) -> dict[str, list[str]]:
    """获取 target 的所有画像"""
    doc = await get_db().users.find_one(
        {"tg_id": target_tg_id},
        {"infos": 1},
    )
    if not doc or "infos" not in doc:
        return {}
    return doc["infos"]


async def replace(target_tg_id: int, author_tg_id: int, items: list[str]) -> bool:
    """整体替换某作者对 target 的描述数组"""
    author_key = str(author_tg_id)
    await get_db().users.update_one(
        {"tg_id": target_tg_id},
        {"$set": {f"infos.{author_key}": items}},
    )
    return True
