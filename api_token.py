"""API Token 管理，用于 HTTP MCP 认证"""

import secrets
from datetime import datetime, timezone
from db import get_db


async def generate(tg_id: int) -> str:
    """为用户生成 API token，替换旧的"""
    token = secrets.token_urlsafe(32)
    await get_db().users.update_one(
        {"tg_id": tg_id},
        {"$set": {"api_token": token, "api_token_created_at": datetime.now(timezone.utc)}},
    )
    return token


async def revoke(tg_id: int) -> bool:
    result = await get_db().users.update_one(
        {"tg_id": tg_id},
        {"$unset": {"api_token": 1, "api_token_created_at": 1}},
    )
    return result.modified_count > 0


async def verify(token: str) -> int | None:
    """验证 token，返回 tg_id 或 None"""
    doc = await get_db().users.find_one({"api_token": token}, {"tg_id": 1})
    return doc["tg_id"] if doc else None
