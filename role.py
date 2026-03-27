"""角色/Skill 模块

roles 集合:
{
    _id: ObjectId,
    name: str,              # 角色名称，如"产品经理"
    trigger: str,           # 触发描述，AI 用来判断何时启用
    prompt: str,            # 完整的角色提示词
    created_by: int,        # 创建者 tg_id
    created_at: datetime,
    updated_at: datetime,
}
"""

from datetime import datetime, timezone
from bson import ObjectId
from db import get_db

# 内存缓存，启动时加载
_roles: list[dict] = []


async def load():
    """启动时从 MongoDB 加载所有角色到内存"""
    global _roles
    cursor = get_db().roles.find({}, {"_id": 1, "name": 1, "trigger": 1, "prompt": 1})
    _roles = await cursor.to_list(length=100)


def get_summary() -> str:
    """返回角色摘要（name + trigger），用于 system prompt"""
    if not _roles:
        return ""
    lines = [f"- {r['name']}：{r['trigger']}" for r in _roles]
    return "\n".join(lines)


def get_by_name(name: str) -> dict | None:
    """按名称查找角色"""
    for r in _roles:
        if r["name"] == name:
            return r
    return None


async def create(name: str, trigger: str, prompt: str, created_by: int) -> str:
    now = datetime.now(timezone.utc)
    result = await get_db().roles.insert_one({
        "name": name, "trigger": trigger, "prompt": prompt,
        "created_by": created_by,
        "created_at": now, "updated_at": now,
    })
    await load()  # 刷新缓存
    return str(result.inserted_id)


async def update(name: str, trigger: str | None = None, prompt: str | None = None) -> bool:
    update_fields = {"updated_at": datetime.now(timezone.utc)}
    if trigger is not None:
        update_fields["trigger"] = trigger
    if prompt is not None:
        update_fields["prompt"] = prompt
    result = await get_db().roles.update_one({"name": name}, {"$set": update_fields})
    if result.modified_count > 0:
        await load()
        return True
    return False


async def delete(name: str) -> bool:
    result = await get_db().roles.delete_one({"name": name})
    if result.deleted_count > 0:
        await load()
        return True
    return False


async def list_all() -> list[dict]:
    return _roles
