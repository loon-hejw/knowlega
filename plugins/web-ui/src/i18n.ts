export type Locale = "zh-CN" | "en-US";

const STORAGE_KEY = "qm.web-ui.locale";
const TEXT_NODE = 4;

const messages: Record<string, string> = {
  Navigation: "导航",
  "Hide sidebar": "隐藏侧栏",
  "Show sidebar": "显示侧栏",
  "Back to admin": "返回管理后台",
  "Color scheme: light / dark / system": "配色方案：浅色 / 深色 / 跟随系统",
  Language: "语言",
  "Sign out": "退出登录",
  "Close sidebar": "关闭侧栏",
  "Resize sidebar": "调整侧栏宽度",
  "Drag to resize · double-click to reset": "拖动调整宽度 · 双击恢复默认",
  "Pick a conversation, or start a new chat.": "选择一个会话，或开始新的聊天。",
  "Viewing the assistant as": "正在以此身份查看助手：",
  "Exit impersonation": "退出模拟身份",
  "Dev mode": "开发模式",
  "Sign in through the portal": "通过门户登录",
  "Your session ended": "会话已结束",
  "You've been signed out. Sign in again and you'll come back to this page.": "你已退出登录。重新登录后会返回此页面。",
  "Sign in": "登录",
  "You don't have access": "你没有访问权限",
  "Your account is signed in and verified — it just isn't allowed on this instance. Ask an administrator to add you.":
    "你的账号已登录并通过验证，但尚未获准访问此实例。请联系管理员添加权限。",
  "We couldn't reach the assistant": "无法连接助手",
  "The service didn't respond. This is usually temporary.": "服务没有响应，通常是暂时性问题。",
  "Try again": "重试",
  "Dev sign-in": "开发环境登录",
  Principal: "用户标识",
  Continue: "继续",
  "Signing in…": "正在登录…",
  "New chat": "新聊天",
  "New session": "新会话",
  Browse: "浏览",
  Projects: "项目",
  Chats: "聊天",
  Files: "文件",
  Crons: "定时任务",
  Keychain: "密钥链",
  Apps: "应用",
  Memory: "记忆",
  Skills: "技能",
  Sessions: "会话",
  "Web only": "仅显示 Web",
  "Showing web chats only": "当前仅显示 Web 聊天",
  "Hide non-web conversations": "隐藏非 Web 会话",
  Pinned: "已置顶",
  Archived: "已归档",
  "Loading conversations…": "正在加载会话…",
  "Loading conversations...": "正在加载会话…",
  "No conversations yet.": "还没有会话。",
  "No conversations yet — start a new chat.": "还没有会话，开始新的聊天吧。",
  "No conversations match.": "没有匹配的会话。",
  "Slack conversations hidden.": "Slack 会话已隐藏。",
  "View project": "查看项目",
  Rename: "重命名",
  "Project options": "项目选项",
  "Conversation options": "会话选项",
  "Copy link": "复制链接",
  Pin: "置顶",
  Unpin: "取消置顶",
  Archive: "归档",
  Unarchive: "取消归档",
  "Refresh title": "刷新标题",
  "Refreshing title": "正在刷新标题",
  "Row color": "行颜色",
  "Custom color (RGB picker)": "自定义颜色（RGB 选择器）",
  "Custom row color": "自定义行颜色",
  "Clear color": "清除颜色",
  "Clear row color": "清除行颜色",
  "Conversation status": "会话状态",
  Active: "进行中",
  Waiting: "等待中",
  Surface: "来源界面",
  "All surfaces": "所有界面",
  Web: "Web",
  Slack: "Slack",
  Personal: "个人",
  "Direct message": "私聊",
  Channel: "频道",
  "Group DM": "群聊",
  "Web chat": "Web 聊天",
  "Shared channel": "共享频道",
  Group: "群组",
  "Agent is working": "助手正在工作",
  "Waiting for your reply": "等待你的回复",
  "just now": "刚刚",
  "Read-only": "只读",
  "New chat in": "在此处新建聊天",
  "New chat in {name}": "在 {name} 中新建聊天",
  "Open here": "在此处打开",
  "Split left": "在左侧拆分",
  "Open full screen": "全屏打开",
  "Close pane": "关闭面板",
  "Open in Slack": "在 Slack 中打开",
  "Loading earlier messages…": "正在加载更早的消息…",
  "Show earlier messages": "显示更早的消息",
  "No readable messages in this conversation.": "此会话中没有可读取的消息。",
  "Loading output…": "正在加载输出…",
  "No output yet": "暂无输出",
  Retry: "重试",
  Send: "发送",
  Cancel: "取消",
  Save: "保存",
  Delete: "删除",
  Close: "关闭",
  Confirm: "确认",
  Refresh: "刷新",
  Search: "搜索",
  Open: "打开",
  Unavailable: "不可用",
  "Load more": "加载更多",
  Loading: "正在加载",
  "Loading…": "正在加载…",
  "Files created, uploaded, or shared with you": "你创建、上传或收到共享的文件",
  Upload: "上传",
  "Add existing": "添加已有文件",
  "Add an existing QM file": "添加已有 QM 文件",
  "No other QM files are available.": "没有其他可添加的 QM 文件。",
  Add: "添加",
  "File queued for knowledge processing.": "文件已进入知识处理队列。",
  "File removed and knowledge base updated.": "文件已移除，知识库已更新。",
  "Couldn't load existing files.": "无法加载已有文件。",
  "Couldn't add that file.": "无法添加该文件。",
  "Couldn't retry that file.": "无法重试该文件。",
  "Couldn't delete that file.": "无法删除该文件。",
  "Drop files here or choose files": "将文件拖到这里，或选择文件",
  "Drop files": "拖放文件",
  Ownership: "所有权",
  "All files": "所有文件",
  Yours: "你的文件",
  Shared: "共享文件",
  Type: "类型",
  "All types": "所有类型",
  Images: "图片",
  Documents: "文档",
  Other: "其他",
  Sort: "排序",
  Newest: "最新",
  Oldest: "最早",
  Name: "名称",
  "No files yet. Upload one here or ask the agent to create one.": "还没有文件。你可以在这里上传，或让助手创建文件。",
  "No files match these filters.": "没有符合这些筛选条件的文件。",
  "No files yet.": "还没有文件。",
  "Copy URL": "复制 URL",
  Copied: "已复制",
  "No accounts available": "没有可用账号",
  "No stored credentials": "没有已保存的凭据",
  "Add one without pasting a secret into chat.": "无需在聊天中粘贴密钥即可添加。",
  "Last used": "最近使用",
  "No audited use yet": "尚无审计使用记录",
  "Your one-time page is ready": "一次性页面已准备好",
  "Open the one-time page": "打开一次性页面",
  Expires: "过期时间",
  Added: "添加时间",
  Disconnect: "断开连接",
  Reconnect: "重新连接",
  Connect: "连接",
  "App view": "应用视图",
  Deploying: "部署中",
  "Can manage": "可管理",
  "Can view": "可查看",
  "Version unknown": "版本未知",
  Restore: "恢复",
  "Edit display name": "编辑显示名称",
  "Change URL slug": "修改 URL 别名",
  "Deploy with Agent": "让助手部署",
  Status: "状态",
  "No apps match your search.": "没有匹配搜索条件的应用。",
  "No apps in this context.": "此上下文中没有应用。",
  "No apps of your own yet.": "你还没有自己的应用。",
  "No apps shared with you.": "没有共享给你的应用。",
  Overview: "概览",
  "Live version": "线上版本",
  "Latest version": "最新版本",
  "Last deployed": "最近部署",
  "Last opened": "最近打开",
  Access: "访问权限",
  "Ownership and access": "所有权和访问权限",
  "Created in": "创建于",
  "Created by": "创建者",
  "No version history available.": "没有可用的版本历史。",
  "Open app": "打开应用",
  "Edit live": "编辑线上版本",
  "Could not load app details.": "无法加载应用详情。",
  "Could not save app settings.": "无法保存应用设置。",
  "Could not archive deployment.": "无法归档部署。",
  "Could not restore deployment.": "无法恢复部署。",
  "Failed to load apps.": "加载应用失败。",
  "No crons yet.": "还没有定时任务。",
  "No active crons.": "没有运行中的定时任务。",
  "None of your own crons yet.": "你还没有自己的定时任务。",
  "No crons shared with you.": "没有共享给你的定时任务。",
  "No runs yet.": "还没有运行记录。",
  Never: "从未",
  Enable: "启用",
  Disable: "停用",
  "run failed": "运行失败",
  "enable failed": "启用失败",
  "disable failed": "停用失败",
  "delete failed": "删除失败",
  "Couldn't load run history.": "无法加载运行历史。",
  "Couldn't load this project's model.": "无法加载此项目的模型。",
  Model: "模型",
  "The model every conversation here starts on.": "此处每个新会话使用的默认模型。",
  "Default model for this project": "此项目的默认模型",
  "Following the org default — it changes when the org's does.": "跟随组织默认设置，组织默认模型变化时会同步变化。",
  "Pinned for this project. Anyone in a chat can still pick a different model for that conversation.":
    "已固定到此项目，但聊天中的任何人仍可为当前会话选择其他模型。",
  Auto: "自动",
  Low: "低",
  Medium: "中",
  High: "高",
  Max: "最高",
  "No remembered facts match this search.": "没有记忆事实符合此搜索。",
  "The agent hasn’t noted any facts yet.": "助手还没有记录任何事实。",
  "automatic capture": "自动记录",
  "Saved ✓": "已保存 ✓",
  "Loading memory history…": "正在加载记忆历史…",
  "Restore memory": "恢复记忆",
  "No skills available yet.": "暂时没有可用技能。",
  "No skills match these filters.": "没有技能符合这些筛选条件。",
  "Created here": "在此创建",
  "Instructions unavailable.": "技能说明不可用。",
  "Loading instructions…": "正在加载技能说明…",
  "Loading skills…": "正在加载技能…",
  "Open project": "打开项目",
  "Loading projects…": "正在加载项目…",
  "No projects yet.": "还没有项目。",
  "Loading this context's files, crons, apps and skills…": "正在加载此上下文的文件、定时任务、应用和技能…",
  "No matches": "没有匹配项",
  "No matches for": "没有匹配项：",
  "Saved.": "已保存。",
  "Failed to load memory.": "加载记忆失败。",
  "Failed to save memory.": "保存记忆失败。",
  "Failed to load skills.": "加载技能失败。",
  "Failed to load files.": "加载文件失败。",
  "Failed to load connectors.": "加载连接器失败。",
  "Failed to load contexts.": "加载上下文失败。",
  "Failed to load crons.": "加载定时任务失败。",
  "Failed to load conversations.": "加载会话失败。",
  "Couldn't load this context's resources.": "无法加载此上下文的资源。",
  "Could not send message.": "无法发送消息。",
  "Could not start the conversation.": "无法开始会话。",
  "Could not fork the conversation.": "无法分叉会话。",
  "Could not attach that file.": "无法附加该文件。",
  "Could not steer the running task.": "无法调整正在运行的任务。",
  "Couldn't load the full output.": "无法加载完整输出。",
  "Accounts and credentials your agent may use on your behalf.": "助手可以代表你使用的账号和凭据。",
  "Active grants": "有效授权",
  "Active only": "仅显示启用项",
  "Add a credential": "添加凭据",
  "Add bot": "添加机器人",
  "Add credential": "添加凭据",
  "Add people": "添加成员",
  "Agent behavior": "助手行为",
  "All contexts": "所有上下文",
  "All scopes": "所有作用域",
  "All sources": "所有来源",
  "Allow always": "始终允许",
  "Allow for session": "本会话允许",
  "Allow once": "允许一次",
  "Ambient behavior": "环境行为",
  "and run the portal to use real sign-in.": "并运行门户以使用真实登录。",
  "API keys, tokens, and files you added through the one-time page.": "你通过一次性页面添加的 API 密钥、令牌和文件。",
  "App actions": "应用操作",
  "Approval needed": "需要批准",
  "Archive and take offline": "归档并下线",
  "Archive cron": "归档定时任务",
  "Archive deployment": "归档部署",
  "Ask the agent to set it up": "让助手设置",
  "Attach files": "附加文件",
  "Authorize access in a new tab": "在新标签页授权访问",
  "Authorized — its tools work here now": "已授权 — 其工具现在可在此处使用",
  "Automated posters": "自动发帖机器人",
  "Available to": "可用对象",
  "Background activity": "后台活动",
  "Bot name": "机器人名称",
  Capabilities: "能力",
  Change: "更改",
  "Changes the app URL. Existing links do not redirect.": "更改应用 URL，现有链接不会重定向。",
  "Check impact": "检查影响",
  "Choose what this project should notice and act on.": "选择此项目应关注和处理的内容。",
  "Clear filters": "清除筛选",
  "Close new project": "关闭新建项目",
  "Command approval": "命令批准",
  "Connected accounts": "已连接账号",
  Conversations: "会话",
  Copy: "复制",
  "Copy app URL": "复制应用 URL",
  "Copy Git remote": "复制 Git 远程地址",
  "Copy message": "复制消息",
  "Create a reusable procedure for yourself or a shared context.": "为自己或共享上下文创建可复用的流程。",
  "Cron actions": "定时任务操作",
  "Cron view": "定时任务视图",
  "Default (on when standing orders are set)": "默认（设置常规指令时开启）",
  "Delete permanently": "永久删除",
  Deny: "拒绝",
  Description: "描述",
  Destination: "目标",
  Details: "详情",
  "Disable cron": "停用定时任务",
  Dismiss: "关闭",
  "Dismiss notification": "关闭通知",
  "Display name": "显示名称",
  Done: "完成",
  "Drop files or folders to attach": "拖放文件或文件夹以附加",
  Edit: "编辑",
  "Edit behavior with agent": "使用助手编辑行为",
  "Edit cron": "编辑定时任务",
  Editing: "编辑中",
  Effort: "努力等级",
  "Enable cron": "启用定时任务",
  "Environment variable": "环境变量",
  Everything: "全部",
  "Expand this pane": "展开此面板",
  Expired: "已过期",
  "Facts the agent carries into your conversations.": "助手在聊天中携带的事实。",
  Fast: "快速",
  "Filter by context": "按上下文筛选",
  "Filter by skill status": "按技能状态筛选",
  "For example: Flag anything that could delay the launch.": "例如：标记可能延误发布的事项。",
  "Forget this fact": "忘记此事实",
  "Fork conversation from here": "从此处分叉会话",
  "Git remote": "Git 远程地址",
  "If this keeps happening, the core service may be down.": "如果持续出现，核心服务可能已停止。",
  "If you opened this surface's own address, that's the cause — it can't authenticate anyone on its own.":
    "如果你打开的是此界面的专属地址，原因就在这里 — 它无法独立验证任何人的身份。",
  "Inherit future defaults": "继承未来默认设置",
  "Insert into message": "插入消息",
  Instructions: "指令",
  "Keep mine": "保留我的设置",
  "Keychain summary": "密钥链摘要",
  "Last fired": "上次触发",
  Latest: "最新",
  "Linked accounts": "已关联账号",
  Live: "线上",
  "Loading authoritative app details…": "正在加载权威应用详情…",
  "Loading runtime settings…": "正在加载运行时设置…",
  "Make default": "设为默认",
  Message: "消息",
  "Narrower scope takes precedence where both apply": "两者都适用时，更窄的作用域优先",
  "Need attention": "需要关注",
  New: "新建",
  "New credential": "新建凭据",
  "New cron": "新建定时任务",
  "New project": "新建项目",
  "New skill": "新建技能",
  "Next run": "下次运行",
  "No bots added. All bot posts are treated as activity.": "尚未添加机器人。所有机器人发帖都会被视为活动。",
  "Nothing running here anymore.": "当前没有正在运行的任务。",
  Now: "现在",
  Off: "关",
  On: "开",
  "One line: what it does / when to use it": "一句话说明用途和适用时机",
  "Open it in a new tab and paste the secret there.": "在新标签页打开，并将密钥粘贴到那里。",
  optional: "可选",
  or: "或",
  Organization: "组织",
  Overrides: "覆盖",
  Owner: "所有者",
  "Pasted text": "已粘贴文本",
  "Pending requests": "待处理请求",
  People: "成员",
  "Plain-language guidance for proactive work. Leave empty to respond only when addressed.":
    "面向用户的主动工作说明。留空则仅在被提及时回复。",
  "Preparing files...": "正在准备文件…",
  "Private channel": "私有频道",
  "Project / group": "项目 / 群组",
  Purpose: "用途",
  "Read-only — replies happen on the original surface": "只读 — 回复会在原界面发送",
  "Recent runs": "最近运行",
  "Reconnect needed": "需要重新连接",
  "Refresh conversations": "刷新会话",
  "Refresh keychain": "刷新密钥链",
  "Refresh memory": "刷新记忆",
  "Refresh projects": "刷新项目",
  Remove: "移除",
  "Restore deployment": "恢复部署",
  "Review again": "再次审核",
  "Revision history": "修订历史",
  "Revision history is unavailable for this memory store.": "此记忆存储没有可用的修订历史。",
  Revoke: "撤销",
  "Run now": "立即运行",
  Schedule: "计划",
  Scope: "作用域",
  "Search by name or handle": "按名称或用户名搜索",
  "Search file names and types…": "搜索文件名和类型…",
  "Search files": "搜索文件",
  "Search memory": "搜索记忆",
  "Search projects": "搜索项目",
  "Search projects…": "搜索项目…",
  "Search remembered facts": "搜索已记忆事实",
  "Secrets stay encrypted and every use or shared grant is audited.": "密钥保持加密，所有使用或共享授权都会被审计。",
  Service: "服务",
  Context: "上下文",
  context: "上下文",
  "Control how messages from bots and integrations wake the agent.": "控制机器人和集成唤醒助手的消息方式。",
  every: "每",
  "Every weekday at 9am, summarize my unread email and DM me the highlights.":
    "每个工作日上午 9 点，总结我的未读邮件并私信要点。",
  "Everyone in a shared context can invoke and edit this skill.": "共享上下文中的每个人都可以调用和编辑此技能。",
  "Everyone in this context can invoke and edit these instructions.": "此上下文中的每个人都可以调用和编辑这些指令。",
  Harness: "运行环境",
  "No identity provider is configured, so this instance trusts a local cookie. Set":
    "未配置身份提供商，因此此实例信任本地 Cookie。设置",
  "Not connected": "未连接",
  "Provider APIs the agent can use as you.": "助手可以代表你使用的提供商 API。",
  "Session settings —": "会话设置 —",
  "This chat runs in the": "此聊天运行在",
  "context — the agent works with that context's files and memory, separate from your personal context.":
    "上下文中 — 助手使用该上下文的文件和记忆，与个人上下文分离。",
  Settings: "设置",
  Show: "显示",
  "Show full command": "显示完整命令",
  "Show full output": "显示完整输出",
  "Skill packs": "技能包",
  Source: "来源",
  "Standing orders": "常规指令",
  Stop: "停止",
  Stopped: "已停止",
  "Stored credentials": "已保存凭据",
  "The human-friendly name shown here. This does not change the app URL.": "此处显示的人性化名称不会更改应用 URL。",
  "The SKILL.md contents — the steps to follow when this skill is used.": "SKILL.md 内容 — 使用此技能时应遵循的步骤。",
  "This instance lists its principals in": "此实例在以下配置中列出用户标识：",
  "This project is ready for work": "此项目已准备好工作",
  Title: "标题",
  "Triggered by": "触发者",
  "Unarchive cron": "取消归档定时任务",
  Undo: "撤销",
  Upgrade: "升级",
  "URL slug": "URL 别名",
  "Use org default": "使用组织默认设置",
  "Use this harness, model, effort, and fast setting as the default for this scope":
    "使用此运行环境、模型、努力等级和快速设置作为此作用域的默认值",
  "Version history": "版本历史",
  "View all": "查看全部",
  "View pasted text": "查看已粘贴文本",
  "View session log (admin)": "查看会话日志（管理员）",
  Worklog: "工作日志",
  Why: "原因",
  "Act immediately": "立即执行",
  "Access revoked ✓": "访问权限已撤销 ✓",
  "Already open in a pane": "已在面板中打开",
  "Another keychain change is still in progress.": "另一个密钥链变更仍在进行中。",
  "Approve or deny to continue": "批准或拒绝后继续",
  "Archiving deployment…": "正在归档部署…",
  "Archiving…": "正在归档…",
  "Ask anything": "随便问",
  "Back to skills": "返回技能",
  "Batch updates": "批量更新",
  "Channels & messages": "频道和消息",
  "Clone or push a new version with this short-lived authenticated URL.": "使用此短期认证 URL 克隆或推送新版本。",
  "Clone source with this short-lived read-only authenticated URL.": "使用此短期只读认证 URL 克隆来源。",
  "Connect account": "连接账号",
  "Context settings": "上下文设置",
  "Core returned an invalid project": "核心服务返回了无效项目",
  "Could not create the one-time page.": "无法创建一次性页面。",
  "Could not delete the key.": "无法删除密钥。",
  "Could not deliver the message — the running task ended mid-send. It is back in the composer.":
    "无法发送消息 — 运行中的任务在发送途中结束，消息已回到输入框。",
  "Could not deliver the message — the running task never settled. It is back in the composer.":
    "无法发送消息 — 运行中的任务一直未完成，消息已回到输入框。",
  "Could not disconnect.": "无法断开连接。",
  "Could not load runtime settings.": "无法加载运行时设置。",
  "Could not open live editing.": "无法打开线上编辑。",
  "Could not reconnect to the running task.": "无法重新连接正在运行的任务。",
  "Could not restore that revision.": "无法恢复该修订版本。",
  "Could not revoke access.": "无法撤销访问权限。",
  "Could not send the approval.": "无法发送批准结果。",
  "Could not start the connector.": "无法启动连接器。",
  "Could not update the scope default.": "无法更新作用域默认设置。",
  "Couldn't add that person.": "无法添加该成员。",
  "Couldn't change the model — try again.": "无法更改模型 — 请重试。",
  "Couldn't create that project.": "无法创建该项目。",
  "Couldn't delete that skill.": "无法删除该技能。",
  "Couldn't load the original conversation's history.": "无法加载原会话历史。",
  "Couldn't load this conversation. Check your connection and click it again.":
    "无法加载此会话。请检查网络连接后再次点击。",
  "Couldn't load this scope's standing orders.": "无法加载此作用域的常规指令。",
  "Couldn't remove that person.": "无法移除该成员。",
  "Couldn't save — try again.": "保存失败 — 请重试。",
  "Couldn't search for people.": "无法搜索成员。",
  "Couldn't update that cron.": "无法更新该定时任务。",
  "Create project": "创建项目",
  "Create skill": "创建技能",
  "Creating…": "正在创建…",
  "Cron updated.": "定时任务已更新。",
  "Delete credential": "删除凭据",
  "Delete this cron? This can't be undone.": "删除此定时任务？此操作无法撤销。",
  "Delete this skill? This can't be undone.": "删除此技能？此操作无法撤销。",
  "Denied.": "已拒绝。",
  "Deploy an app for me.": "帮我部署一个应用。",
  "Deployment time unavailable": "部署时间不可用",
  "Describe the cron you want.": "描述你想要的定时任务。",
  "Discard and refresh": "放弃并刷新",
  "Discard unsaved memory changes?": "放弃未保存的记忆更改？",
  "Disconnect account": "断开账号连接",
  "Edit notebook": "编辑笔记本",
  "Encrypted at rest": "静态加密",
  "Enter a project name.": "输入项目名称。",
  "Enter at least two characters.": "至少输入两个字符。",
  "Everyone matching is already in this project.": "符合条件的成员都已在此项目中。",
  "Failed to archive skill.": "归档技能失败。",
  "Failed to create skill.": "创建技能失败。",
  "Failed to load all matching files.": "加载所有匹配文件失败。",
  "Failed to load background activity.": "加载后台活动失败。",
  "Failed to load memory history.": "加载记忆历史失败。",
  "Failed to load more files.": "加载更多文件失败。",
  "Failed to load skill details.": "加载技能详情失败。",
  "Failed to restore skill.": "恢复技能失败。",
  "Failed to save skill.": "保存技能失败。",
  "Fast mode": "快速模式",
  "Fast mode active": "快速模式已启用",
  "Fast mode is only available on Opus models": "快速模式仅适用于 Opus 模型",
  "Files & folders": "文件和文件夹",
  "Filter skills by scope": "按作用域筛选技能",
  "Filter skills by source": "按来源筛选技能",
  "Finished step": "步骤已完成",
  "Focus this pane over the grid": "将此面板置于网格上层",
  "Gmail, Calendar, Drive, Sheets": "Gmail、日历、云端硬盘、表格",
  "Hide background activity": "隐藏后台活动",
  "Hide disabled": "隐藏已停用项",
  "Hide output": "隐藏输出",
  "Issues & projects": "问题和项目",
  "Just you — your web chats and DMs with the agent live here.": "仅你可见 — 你与助手的 Web 聊天和私聊会显示在这里。",
  "Loading apps…": "正在加载应用…",
  "Loading crons…": "正在加载定时任务…",
  "Loading files…": "正在加载文件…",
  "Loading keychain…": "正在加载密钥链…",
  "Loading runtime…": "正在加载运行时设置…",
  "Loading skill instructions…": "正在加载技能指令…",
  "Loading your keychain…": "正在加载你的密钥链…",
  "Managing process": "正在管理进程",
  "Managed process": "受管进程",
  "Needs your approval": "需要你的批准",
  "Never fired": "从未触发",
  "No active run to signal.": "没有可通知的活动运行。",
  "No authorization URL was returned.": "未返回授权 URL。",
  "No crons in this context.": "此上下文中没有定时任务。",
  "No one-time page URL was returned.": "未返回一次性页面 URL。",
  "No projects match your search.": "没有匹配搜索条件的项目。",
  "No recorded access": "没有记录的访问",
  "Nothing archived.": "没有已归档内容。",
  "None required": "无需任何能力",
  "Open in new tab": "在新标签页打开",
  "Org default": "组织默认",
  "Pages & databases": "页面和数据库",
  "Personal — only you": "个人 — 仅你可见",
  "Posts & profile": "帖子和个人资料",
  "Previous 30 days": "过去 30 天",
  "Previous 7 days": "过去 7 天",
  "Project settings": "项目设置",
  "Publish change": "发布更改",
  "Publish skill": "发布技能",
  "Ran command": "已运行命令",
  "Read file": "已读取文件",
  "Reading file": "正在读取文件",
  "Restore revision": "恢复修订版本",
  "Restore to grid (Esc)": "恢复到网格（Esc）",
  "Restoring deployment…": "正在恢复部署…",
  "Revision restored ✓": "修订版本已恢复 ✓",
  "Revision restored ✓ History could not refresh.": "修订版本已恢复 ✓ 历史记录无法刷新。",
  "Revoke access": "撤销访问权限",
  "Run started. Refresh recent runs after it completes.": "运行已开始。完成后刷新最近运行记录。",
  "Running command": "正在运行命令",
  "Save changes": "保存更改",
  "Saving…": "正在保存…",
  "Scope variant": "作用域变体",
  "Search apps": "搜索应用",
  "Search chats…": "搜索聊天…",
  "Search crons": "搜索定时任务",
  "Search skills…": "搜索技能…",
  "Searched history": "已搜索历史记录",
  "Searched memory": "已搜索记忆",
  "Searching history": "正在搜索历史记录",
  "Searching memory": "正在搜索记忆",
  "Searching…": "正在搜索…",
  "Service and purpose are required.": "服务和用途为必填项。",
  "Shared context": "共享上下文",
  "Shared with everyone in this channel.": "与此频道中的所有人共享。",
  "Shared with everyone in this group conversation.": "与此群组会话中的所有人共享。",
  "Show disabled": "显示已停用项",
  "Show less": "收起",
  "Show live output": "显示实时输出",
  "Show more": "展开",
  "Sign-in failed.": "登录失败。",
  "Split down": "向下拆分",
  "Split right": "向右拆分",
  "Split this pane with a new session": "使用新会话拆分此面板",
  "Split up": "向上拆分",
  "Steer the running task": "调整正在运行的任务",
  "Steer the running task (attachments stay for your next message)": "调整正在运行的任务（附件会保留到下一条消息）",
  "Steer the running task…": "调整正在运行的任务…",
  "Still preparing the previous drop — try again in a moment.": "上一次拖放仍在准备中 — 请稍后重试。",
  "Still syncing this conversation — try again in a moment": "此会话仍在同步 — 请稍后重试",
  "That conversation wasn't found, or you don't have access to it.": "找不到该会话，或你无权访问。",
  "That cron wasn't found, or you don't have access to it.": "找不到该定时任务，或你无权访问。",
  "That drop included a folder this browser can't read — zip it and drop the archive instead.":
    "拖放内容包含此浏览器无法读取的文件夹 — 请压缩后再拖放压缩包。",
  "The agent run failed.": "助手运行失败。",
  "The selected notebook will become current. The version you have now remains available in history.":
    "选中的笔记本将成为当前版本。当前版本仍会保留在历史记录中。",
  "This conversation is read-only here.": "此处为只读会话。",
  "This edit link is missing a valid app name.": "此编辑链接缺少有效的应用名称。",
  "Timed out waiting for the agent to respond.": "等待助手响应超时。",
  "Title and task are required.": "标题和任务为必填项。",
  "Title is required.": "标题为必填项。",
  "Unsaved changes": "未保存的更改",
  "Upload failed.": "上传失败。",
  "Uploading…": "正在上传…",
  "Using memory": "正在使用记忆",
  "Using URL slug": "正在使用 URL 别名",
  Task: "任务",
  Team: "团队",
  "What may the agent use this credential for?": "此凭据可供助手用于什么？",
  "Your workspace has not configured any account providers yet.": "你的工作区尚未配置任何账号提供商。",
  "Want to get set up? Tell me your name and what you're working on, and I'll take it from there — or just ask me anything to dive straight in.":
    "想要开始吗？告诉我你的名字和正在做什么，我会从这里接手 — 或者直接问我任何问题。",
  "Working…": "正在工作…",
  "Writing file": "正在写入文件",
  "Wrote file": "已写入文件",
  "Your one-time page is ready.": "一次性页面已准备好。",
  "Your personal context": "你的个人上下文",
};

const patterns: Array<[RegExp, (match: RegExpMatchArray) => string]> = [
  [/^(\d+)m ago$/, (m) => `${m[1]} 分钟前`],
  [/^(\d+)h ago$/, (m) => `${m[1]} 小时前`],
  [/^(\d+)d ago$/, (m) => `${m[1]} 天前`],
  [/^New chat in (.+)$/, (m) => `在 ${m[1]} 中新建聊天`],
  [/^Options for (.+)$/, (m) => `${m[1]} 的选项`],
  [/^Copy link to (.+)$/, (m) => `复制 ${m[1]} 的链接`],
  [/^(Unpin|Pin|Archive|Unarchive) (.+)$/, (m) => `${messages[m[1]!] ?? m[1]} ${m[2]}`],
  [/^Color row (.+)$/, (m) => `为行设置颜色 ${m[1]}`],
  [/^(.+) — click to inspect$/, (m) => `${m[1]} — 点击查看`],
  [/^Session settings — (.+)$/, (m) => `会话设置 — ${m[1]}`],
  [/^Showing (\d+) of (\d+)$/, (m) => `显示 ${m[1]} / ${m[2]}`],
  [/^Uploaded (\d+) files?\.$/, (m) => `已上传 ${m[1]} 个文件。`],
  [/^Uploading (\d+) files?…$/, (m) => `正在上传 ${m[1]} 个文件…`],
  [/^Uploaded (\d+) of (\d+)\.$/, (m) => `已上传 ${m[1]} / ${m[2]}。`],
  [
    /^Knowledge: (queued|processing|ready|failed|unsupported|removing)$/,
    (m) =>
      `知识：${({ queued: "排队中", processing: "处理中", ready: "已就绪", failed: "失败", unsupported: "不支持", removing: "删除中" } as Record<string, string>)[m[1]!] ?? m[1]}`,
  ],
  [/^(\d+) pages$/, (m) => `${m[1]} 个页面`],
  [/^(.+) added to this project\.$/, (m) => `${m[1]} 已添加到此项目。`],
  [/^Delete (.+) from this project and update its knowledge base\?$/, (m) => `从此项目删除 ${m[1]} 并更新其知识库？`],
  [/^Open (.+)$/, (m) => `打开 ${m[1]}`],
  [/^Search (.+)$/, (m) => `搜索${m[1]}`],
];

let locale: Locale = detectLocale();
const localeListeners = new Set<(locale: Locale) => void>();
const originalText = new WeakMap<Text, string>();
const renderedText = new WeakMap<Text, string>();
const originalAttributes = new WeakMap<Element, Map<string, string>>();
const renderedAttributes = new WeakMap<Element, Map<string, string>>();

export function detectLocale(): Locale {
  try {
    const saved = localStorage.getItem(STORAGE_KEY);
    if (saved === "zh-CN" || saved === "en-US") return saved;
  } catch {
    void 0;
  }
  const languages = typeof navigator !== "undefined" ? (navigator.languages ?? [navigator.language]) : [];
  return languages.some((value) => value?.toLowerCase().startsWith("zh")) ? "zh-CN" : "en-US";
}

export function getLocale(): Locale {
  return locale;
}

export function setLocale(next: Locale): void {
  locale = next;
  try {
    localStorage.setItem(STORAGE_KEY, next);
  } catch {
    void 0;
  }
  if (typeof document !== "undefined") document.documentElement.lang = next;
  for (const listener of localeListeners) listener(next);
}

export function subscribeLocale(listener: (locale: Locale) => void): () => void {
  localeListeners.add(listener);
  return () => localeListeners.delete(listener);
}

export function t(source: string, params: Record<string, string | number> = {}): string {
  const translated = locale === "zh-CN" ? (messages[source] ?? translatePattern(source) ?? source) : source;
  return translated.replace(/\{(\w+)\}/g, (_, key: string) => String(params[key] ?? `{${key}}`));
}

function translatePattern(source: string): string | undefined {
  for (const [pattern, render] of patterns) {
    const match = source.match(pattern);
    if (match) return render(match);
  }
  return undefined;
}

function splitWhitespace(value: string): { lead: string; body: string; tail: string } {
  const lead = value.match(/^\s*/)?.[0] ?? "";
  const tail = value.match(/\s*$/)?.[0] ?? "";
  return { lead, body: value.slice(lead.length, value.length - tail.length || undefined), tail };
}

function shouldSkip(element: Element): boolean {
  if (element.hasAttribute("data-i18n-skip")) return true;
  const tag = element.tagName.toLowerCase();
  if (["script", "style", "textarea", "input", "pre", "code"].includes(tag)) return true;
  return Boolean(element.closest(".markdown, .transcript, .message, .user-name, .list-row-title"));
}

function translateTextNode(node: Text): void {
  const value = node.textContent ?? "";
  const previous = renderedText.get(node);
  const source = previous !== undefined && value === previous ? (originalText.get(node) ?? value) : value;
  originalText.set(node, source);
  const parts = splitWhitespace(source);
  const output = `${parts.lead}${t(parts.body)}${parts.tail}`;
  if (node.textContent !== output) node.textContent = output;
  renderedText.set(node, output);
}

function translateAttributes(element: Element): void {
  if (shouldSkip(element)) return;
  const names = ["title", "aria-label", "placeholder"];
  let originals = originalAttributes.get(element);
  if (!originals) {
    originals = new Map();
    originalAttributes.set(element, originals);
  }
  let rendered = renderedAttributes.get(element);
  if (!rendered) {
    rendered = new Map();
    renderedAttributes.set(element, rendered);
  }
  for (const name of names) {
    const value = element.getAttribute(name);
    if (value === null) continue;
    const previous = originals.get(name);
    const previousRendered = rendered.get(name);
    const source = previous !== undefined && value === previousRendered ? previous : value;
    originals.set(name, source);
    const output = t(source);
    if (value !== output) element.setAttribute(name, output);
    rendered.set(name, output);
  }
}

export function translateDom(root: ParentNode): void {
  const doc = root.ownerDocument ?? (typeof document !== "undefined" ? document : null);
  if (!doc) return;
  const walker = doc.createTreeWalker(root, TEXT_NODE);
  const nodes: Text[] = [];
  for (let node = walker.nextNode(); node; node = walker.nextNode()) nodes.push(node as Text);
  for (const node of nodes) {
    const parent = node.parentElement;
    if (parent && !shouldSkip(parent)) translateTextNode(node);
  }
  const elements =
    root.nodeType === 1
      ? [root as Element, ...Array.from(root.querySelectorAll("*"))]
      : Array.from(root.querySelectorAll("*"));
  for (const element of elements) translateAttributes(element);
}

export function installDomTranslator(root: HTMLElement): () => void {
  if (typeof document !== "undefined") document.documentElement.lang = locale;
  let scheduled = false;
  const flush = () => {
    scheduled = false;
    translateDom(root);
  };
  const observer =
    typeof MutationObserver === "undefined"
      ? null
      : new MutationObserver(() => {
          if (scheduled) return;
          scheduled = true;
          queueMicrotask(flush);
        });
  observer?.observe(root, {
    subtree: true,
    childList: true,
    characterData: true,
    attributes: true,
    attributeFilter: ["title", "aria-label", "placeholder"],
  });
  const unsubscribe = subscribeLocale(() => translateDom(root));
  translateDom(root);
  return () => {
    observer?.disconnect();
    unsubscribe();
  };
}
