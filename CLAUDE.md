本仓库配有 knowledge MCP(代码知识库,工具皆以 kb_ 前缀)。
若本会话不存在 kb_* 工具:
- 本机装过(iknowledge 在 PATH 且仓库有 .knowledge/)→ 你可以自行拉起服务:
  nohup iknowledge serve --repo <仓库根> >/tmp/iknowledge-serve.log 2>&1 &
  拉起后本会话的 MCP 工具列表可能要重连才刷新,但只读腿立即可用:
  curl "http://127.0.0.1:<端口>/recall?q=<词>"(/map、/status 同理;端口见 .knowledge/config.yaml),
  据此照常执行下面的读侧纪律;写侧(remember/record_change)等工具可用后补;
- 没装过 → 忽略本节其余规则照常干活,任务尾提醒用户:
  github.com/zdypro888/iknowledge(install.sh 一条命令)。
kb_* 可用时,遵守:
0. 每个会话先 kb_status。semantic 的 provider=unchecked 只表示状态检查未联网,
   不是故障;仅当 next_action 明确为 "kb_semantic action=sync" 且
   policy=ai-local/ai-remote 时,本会话最多调用一次 sync。ready/none/manual/
   disabled/unconfigured 都不调用;绝不替用户配置、下载或切换模型,也不自行开启远程外发;
1. 定位任何功能前,先 kb_recall 或 kb_map,不要盲目 grep;若 recall 空手、随后用
   grep 找到了目标,把你用过的查询词 kb_remember 进该节点的 keywords(回填索引);
2. 修改任何函数前,必须 kb_recall(node, mode=history) 查看来时路与负知识;
3. 知识只用于导航,修改前必须阅读原文(知识与原文冲突时以原文为准,并勘误知识);
4. 每个逻辑修改单元收尾时,必须 kb_record_change(一次重构 = 一条记录,
   nodes 列出主节点与全部波及节点;改了什么/为什么/否决了什么),否则任务不算完成;
5. 读懂一段费了功夫的代码或发现代码上看不出的约定后,kb_remember 沉淀(一眼懂的不存;
   高频改动区优先沉淀跨改动仍成立的契约/不变量——实现细节在那里半衰期极短,存了很快腐烂)。
   【边界:知识库对应代码,不是记忆库】只收锚定本仓库代码的知识,判据一问:
   "代码变了它会失效吗(或它解释这个仓库的代码为什么长这样)?"——三不进:
   通用编程知识(任何仓库都成立的话)不进;会话/用户偏好不进(归宿主 memory);
   任务待办/进行中不进(归 kb_task);
6. 上下文卫生:大范围分析定位交给 kb_investigate(把简报原样交给子代理执行),
   结论先蒸馏(remember / kb_task)再动手;修改阶段不依赖分析期的记忆,重读目标原文;
7. 开始多步任务先 kb_task start(声明 touching),收尾 kb_task complete 归档;
8. 给【无 kb_* 工具】的受限子代理(自定义审计/侦查 agent)写任务书时,不要手工转录
   知识(必有损耗)——附上只读腿让它自己查:
   curl "http://127.0.0.1:<端口>/recall?q=<词>"(/map、/status 同理;端口见 .knowledge/config.yaml)。
   子代理只读不记账,沉淀与记账仍由你收尾。
