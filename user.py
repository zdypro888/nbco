from dataclasses import dataclass


@dataclass
class User:
    tg_id: int
    is_superadmin: bool = False
    has_profile: bool = False
    has_admin: bool = False  # 是否有任何管理权限
