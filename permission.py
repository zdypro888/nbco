"""双维度权限系统

主动权限 (active_perms): 我能对谁做什么，存在操作者身上
被动权限 (passive_perms): 谁能对我做什么，存在被操作者身上

超管绕过所有检查。
"""

from db import get_db

ACTIVE_ACTIONS = {"write_profile", "view_self_intro", "manage_perm", "generate_key", "send_msg", "create_project", "edit_info"}


def is_valid_passive_action(action: str) -> bool:
    """校验被动权限 action 格式"""
    if action == "view_profile:_all":
        return True
    if action.startswith("view_profile:"):
        suffix = action[len("view_profile:"):]
        return suffix.isdigit()
    return False


# ===== 通用内部方法 =====

async def _get_perms(tg_id: int, field: str) -> dict:
    doc = await get_db().users.find_one({"tg_id": tg_id}, {field: 1})
    if not doc or field not in doc:
        return {}
    return doc[field]


async def _grant(tg_id: int, field: str, key: str, action: str):
    await get_db().users.update_one(
        {"tg_id": tg_id},
        {"$addToSet": {f"{field}.{key}": action}},
    )


async def _revoke(tg_id: int, field: str, key: str, action: str):
    await get_db().users.update_one(
        {"tg_id": tg_id},
        {"$pull": {f"{field}.{key}": action}},
    )


def _has_action(perms: dict, key: str, action: str) -> bool:
    """检查 perms 中 key 或 _all 是否包含 action"""
    if key in perms and action in perms[key]:
        return True
    if "_all" in perms and action in perms["_all"]:
        return True
    return False


# ===== 主动权限 =====

async def check_active(subject_tg_id: int, action: str, target_tg_id: int, is_superadmin: bool = False) -> bool:
    """检查 subject 是否有对 target 的主动权限"""
    if is_superadmin:
        return True
    perms = await _get_perms(subject_tg_id, "active_perms")
    return _has_action(perms, str(target_tg_id), action)


async def grant_active(subject_tg_id: int, action: str, target_tg_id: int | str) -> bool:
    """给 subject 授予对 target 的主动权限"""
    if action not in ACTIVE_ACTIONS:
        return False
    key = "_all" if target_tg_id == "_all" else str(target_tg_id)
    await _grant(subject_tg_id, "active_perms", key, action)
    return True


async def revoke_active(subject_tg_id: int, action: str, target_tg_id: int | str) -> bool:
    key = "_all" if target_tg_id == "_all" else str(target_tg_id)
    await _revoke(subject_tg_id, "active_perms", key, action)
    return True


async def list_active(tg_id: int) -> dict:
    return await _get_perms(tg_id, "active_perms")


async def inherit_view_perms(from_tg_id: int, to_tg_id: int, is_superadmin: bool = False):
    """把 from 的 view_self_intro 权限范围继承给 to"""
    if is_superadmin:
        # 超管分配：给被分配人 view_self_intro: _all
        await grant_active(to_tg_id, "view_self_intro", "_all")
        return
    perms = await _get_perms(from_tg_id, "active_perms")
    # 收集 from 能看的人
    targets = set()
    if "_all" in perms and "view_self_intro" in perms["_all"]:
        await grant_active(to_tg_id, "view_self_intro", "_all")
        return
    for key, actions in perms.items():
        if "view_self_intro" in actions:
            targets.add(key)
    for target_key in targets:
        await grant_active(to_tg_id, "view_self_intro", target_key if target_key == "_all" else int(target_key))


# ===== 被动权限 =====

async def check_passive(target_tg_id: int, action: str, subject_tg_id: int, is_superadmin: bool = False) -> bool:
    """检查 target 是否允许 subject 执行 action（被动权限在 target 身上）"""
    if is_superadmin:
        return True
    perms = await _get_perms(target_tg_id, "passive_perms")
    return _has_action(perms, str(subject_tg_id), action)


async def grant_passive(target_tg_id: int, action: str, subject_tg_id: int | str) -> bool:
    """给 target 添加被动权限，允许 subject 执行 action"""
    if not is_valid_passive_action(action):
        return False
    key = "_all" if subject_tg_id == "_all" else str(subject_tg_id)
    await _grant(target_tg_id, "passive_perms", key, action)
    return True


async def revoke_passive(target_tg_id: int, action: str, subject_tg_id: int | str) -> bool:
    if not is_valid_passive_action(action):
        return False
    key = "_all" if subject_tg_id == "_all" else str(subject_tg_id)
    await _revoke(target_tg_id, "passive_perms", key, action)
    return True


async def list_passive(tg_id: int) -> dict:
    return await _get_perms(tg_id, "passive_perms")


# ===== 画像可见性判定 =====

async def can_view_infos(viewer_tg_id: int, target_tg_id: int, author_tg_id: int, is_superadmin: bool = False) -> bool:
    """viewer 能否看到 target 身上 author 写的画像"""
    # 超管看全部
    if is_superadmin:
        return True
    # 作者本人能看自己写的
    if viewer_tg_id == author_tg_id:
        return True
    # 目标的自我介绍 + viewer 有 view_self_intro 主动权限
    if author_tg_id == target_tg_id:
        return await check_active(viewer_tg_id, "view_self_intro", target_tg_id)
    # 检查 target 的被动权限：是否允许 viewer 看 author 写的
    perms = await _get_perms(target_tg_id, "passive_perms")
    # 精确检查 view_profile:author_id
    if _has_action(perms, str(viewer_tg_id), f"view_profile:{author_tg_id}"):
        return True
    # 通配检查 view_profile:_all
    if _has_action(perms, str(viewer_tg_id), "view_profile:_all"):
        return True
    return False


# ===== 便捷方法 =====

async def get_manageable_targets(subject_tg_id: int, action: str, is_superadmin: bool = False) -> list[int] | str:
    """获取 subject 可以对哪些人执行 action"""
    if is_superadmin:
        return "_all"
    perms = await _get_perms(subject_tg_id, "active_perms")
    if "_all" in perms and action in perms["_all"]:
        return "_all"
    targets = []
    for key, actions in perms.items():
        if key != "_all" and action in actions:
            targets.append(int(key))
    return targets
