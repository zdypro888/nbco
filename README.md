# TGCompany - 内部管理系统

基于 Telegram Bot + Claude AI 的软件开发公司内部管理工具。员工通过 Telegram 自然语言交互，AI 自动调用业务工具完成管理操作。

## 功能

- **用户管理** — Key 邀请绑定、基本信息维护、用户禁用
- **画像系统** — 自我介绍 + 他人评价，AI 归纳去重，所有权归作者
- **双维度权限** — 主动权限（我能对谁做什么）+ 被动权限（谁能对我做什么），精确到人
- **项目管理** — 创建项目、分配任务、多级拆分下发
- **任务系统** — checklist 工作清单、进度日志、状态管理、向下溯源
- **角色/Skill** — 可自定义 AI 角色（产品经理、HR 等），按需激活
- **消息推送** — 任务分配/状态变化自动通知

## 技术栈

- Python 3.13
- Claude Agent SDK（`@tool` 装饰器暴露业务能力）
- python-telegram-bot（Bot API）
- MongoDB（motor 异步驱动）

## 项目结构

```
bot.py          TG Bot 入口，消息路由
ai.py           Claude 对话，动态组合 tools 和 prompt
auth.py         用户鉴权 + 内存缓存
permission.py   双维度权限校验
tools.py        所有 @tool 定义（user/admin/telegram）
task.py         项目 + 任务模块
profile.py      画像模块
role.py         角色/Skill 模块
key.py          绑定 Key 管理
notify.py       通知推送
user.py         User 数据类
db.py           MongoDB 连接
config.json     配置文件（不提交）
```

## 配置

创建 `config.json`：

```json
{
  "bot_token": "your-telegram-bot-token",
  "superadmins": [123456789],
  "mongodb_uri": "mongodb://localhost:27017",
  "mongodb_db": "nbco"
}
```

## 启动

```bash
python3.13 -m venv .venv
source .venv/bin/activate
pip install python-telegram-bot claude-agent-sdk motor
python bot.py
```

## 权限体系

### 主动权限（active_perms）
| 类型 | 说明 |
|------|------|
| write_profile | 给目标写画像 |
| view_self_intro | 看目标的自我介绍 |
| manage_perm | 管理目标的权限 |
| generate_key | 生成绑定 Key |
| send_msg | 给目标发消息 |
| create_project | 创建项目并分配任务 |
| split_task | 拆分任务并下发 |

### 被动权限（passive_perms）
| 类型 | 说明 |
|------|------|
| view_profile:作者id | 允许某人看指定作者写的画像 |
| view_profile:_all | 允许某人看所有人写的画像 |

超级管理员自动绕过所有权限检查。
