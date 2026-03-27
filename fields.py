"""基本信息字段定义，存 MongoDB，超管可动态管理

fields 集合:
{
    _id: "config",
    fields: ["name", "nickname", "phone", "email", "position"]
}
"""

from db import get_db

_DOC_ID = "config"
_DEFAULT_FIELDS = ["name", "nickname", "phone", "email", "position"]

# 内存缓存
_fields: list[str] = []


async def load():
    global _fields
    doc = await get_db().fields.find_one({"_id": _DOC_ID})
    if doc:
        _fields = doc["fields"]
    else:
        _fields = list(_DEFAULT_FIELDS)
        await get_db().fields.insert_one({"_id": _DOC_ID, "fields": _fields})


def get_fields() -> list[str]:
    return _fields


async def add_field(name: str) -> bool:
    name = name.strip().lower()
    if not name or name in _fields:
        return False
    _fields.append(name)
    await _save()
    return True


async def remove_field(name: str) -> bool:
    name = name.strip().lower()
    if name not in _fields:
        return False
    _fields.remove(name)
    await _save()
    return True


async def _save():
    await get_db().fields.update_one(
        {"_id": _DOC_ID},
        {"$set": {"fields": _fields}},
    )
