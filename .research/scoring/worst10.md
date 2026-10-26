# 最差 10 个节点（该切谁）

参考时间 2026-10-20T23:07:10+08:00（按分数升序，同分按原始分升序）

| 分数 | 节点 | 分档 | 尝试/有效样本 | p95 时长(含生成) | 最后错误 | 扣分原因 |
|---|---|---|---|---|---|---|
| 8.1 | 飞鸟云::美国02aws | 停用 | 148 / 41 | 225s | probe: Get "https://opencode.ai/zen/v1/model | 探测判死：Get "https://opencode.ai/zen/v1/models": timeout: no recent network activity；连接失败 128 次（占尝试 86.5%）；被拒/凭据失效 15 次（10.1%：403 FreeTier / 401 / 410）；真 |
| 9.5 | 飞鸟云::新加坡06aws | 停用 | 164 / 33 | 278s | probe: Get "https://opencode.ai/zen/v1/model | 探测判死：Get "https://opencode.ai/zen/v1/models": timeout: no recent network activity；连接失败 127 次（占尝试 77.4%）；被 freetier 校验确认为坏节点 1 次；真实调用成功率下界仅 38%（有效样本 33 |
| 11.2 | 2::US-198.46.161.42:5092 | 停用 | 16 / 11 | - | probe: Get "https://opencode.ai/zen/v1/model | 探测判死：Get "https://opencode.ai/zen/v1/models": context deadline exceeded；连接失败 16 次（占尝试 100.0%）；真实调用成功率下界仅 47%（有效样本 11）；有效样本不足（n_eff=11 < 20），置信度低 |
| 11.4 | 飞鸟云::hy2台湾06 | 停用 | 387 / 58 | 138s | probe: Get "https://opencode.ai/zen/v1/model | 探测判死：Get "https://opencode.ai/zen/v1/models": timeout: no recent network activity；连接失败 185 次（占尝试 47.8%）；被 freetier 校验确认为坏节点 1 次；被拒/凭据失效 1 次（0.3%：403 F |
| 11.6 | 飞鸟云::美国03aws | 停用 | 51 / 3 | 260s | probe: Get "https://opencode.ai/zen/v1/model | 探测判死：Get "https://opencode.ai/zen/v1/models": timeout: no recent network activity；连接失败 49 次（占尝试 96.1%）；有效样本不足（n_eff=3 < 20），置信度低；最近一次真实成功在 765 小时前（数据陈 |
| 12.4 | 16548d7f | 停用 | 0 / 0 | - | probe: Get "https://opencode.ai/zen/v1/model | 探测判死：Get "https://opencode.ai/zen/v1/models": timeout: no recent network activity；有效样本不足（n_eff=0 < 20），置信度低 |
| 12.4 | 1a315abf | 停用 | 0 / 0 | - | probe: Get "https://opencode.ai/zen/v1/model | 探测判死：Get "https://opencode.ai/zen/v1/models": 209.248.40.224:11442 connect error: remote error:；有效样本不足（n_eff=0 < 20），置信度低 |
| 12.4 | 1a45dab9 | 停用 | 0 / 0 | - | probe: Get "https://opencode.ai/zen/v1/model | 探测判死：Get "https://opencode.ai/zen/v1/models": context deadline exceeded；有效样本不足（n_eff=0 < 20），置信度低 |
| 12.4 | 47a38561 | 停用 | 0 / 0 | - | probe: Get "https://opencode.ai/zen/v1/model | 探测判死：Get "https://opencode.ai/zen/v1/models": 209.248.40.224:20909 connect error: EOF；有效样本不足（n_eff=0 < 20），置信度低 |
| 12.4 | 4a77bbb1 | 停用 | 0 / 0 | - | probe: Get "https://opencode.ai/zen/v1/model | 探测判死：Get "https://opencode.ai/zen/v1/models": 209.248.40.224:48855 connect error: remote error:；有效样本不足（n_eff=0 < 20），置信度低 |

> 说明：`dead` 节点分数 = 原始分 × 0.30（保留序），所以同一档内分数仍有区分度。
