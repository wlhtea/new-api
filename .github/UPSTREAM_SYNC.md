# 上游同步机制说明（UPSTREAM SYNC）

本 fork（`wlhtea/new-api`）在上游 [QuantumNous/new-api](https://github.com/QuantumNous/new-api) 基础上定制了 **Seed Dance（即梦/豆包视频生成）渠道** 等功能。为了在保留定制功能的同时持续跟进上游更新，仓库配置了自动同步机制（`.github/workflows/sync-upstream.yml`）。

## 分支模型

| 分支 | 作用 | 规则 |
|------|------|------|
| `main` | 上游镜像 | 永远与上游 `main` 完全一致，由 workflow 每周强制同步。**不要在这个分支上提交任何代码**，否则会被覆盖。 |
| `custom-main` | 定制主分支（默认分支） | 所有定制功能都在这里，部署也从这里出。上游更新会持续合入本分支。日常开发直接基于它。 |

```
QuantumNous/new-api (上游 main)
        │  每周一 06:00 (北京时间) 自动镜像
        ▼
wlhtea/new-api : main（纯镜像，勿动）
        │  无冲突 → 自动合并推送
        │  有冲突 → 自动开 PR，等待手动解决
        ▼
wlhtea/new-api : custom-main（定制代码 + 上游更新）
```

## 自动同步流程

每周一北京时间早上 6 点（或在 Actions 页面手动触发 **Sync Upstream**）：

1. 把上游 `main` 强制推送到 fork 的 `main`（保持纯镜像）；
2. 检查上游是否有 `custom-main` 还没有的新提交，没有则直接结束；
3. 尝试把上游合并进 `custom-main`：
   - **无冲突** → 自动合并并推送，无需任何人工操作；
   - **有冲突** → 创建一个 `main -> custom-main` 的 PR，描述中列出冲突文件清单和解决步骤。

每次运行结果会写在该次 workflow 运行页面的 Summary 里。

## 出现冲突 PR 时怎么办

在本地执行：

```bash
git fetch origin
git switch custom-main
git pull origin custom-main
git merge origin/main        # 触发和 PR 中相同的冲突
# 逐个文件解决冲突（保留定制逻辑 + 采纳上游改动）
git add -A && git commit
git push origin custom-main
```

推送后 GitHub 会自动把该 PR 标记为已合并。定制代码集中在 `relay/channel/task/seedance/`、`dto/`、`controller/` 及 `web/src` 的渠道配置部分，冲突多发生在渠道注册表、路由、常量等共享文件，解决时注意同时保留上游新增渠道与 Seed Dance 渠道。

## 一次性设置（首次启用必读）

1. **推送分支**（在本机执行）：
   ```bash
   cd /Users/lanhaowu/Documents/ali-video/new-api
   git push fork custom-main
   ```
2. **设为默认分支**：GitHub 仓库页 → Settings → General → Default branch → 切换为 `custom-main`。
   ⚠️ 定时任务只在默认分支上生效，此步骤不可省略。
3. **启用 Actions**：fork 仓库的 Actions 页签 → 点击启用（fork 默认禁用定时 workflow）。
4. **验证**：Actions → Sync Upstream → Run workflow 手动跑一次，确认 Summary 输出正常。

## 注意事项

- fork 中来自上游的其他 workflow（docker 构建、release 等）大多由 tag 或特定事件触发，一般不会误跑；如有多余运行，可在 Actions 页面逐个 Disable。
- 本 workflow 不会同步上游的 tag，因此不会意外触发 fork 的 release/docker 构建。
- GitHub 规定：仓库 60 天无任何提交活动会自动暂停定时 workflow（会发邮件提醒）。本仓库每周有自动合并提交，正常情况下不会触发；若收到暂停邮件，去 Actions 页面重新启用即可。
- 自动创建的冲突 PR 由 `GITHUB_TOKEN` 发起，不会触发 `pr-check` 等 CI；合并上游后建议本地跑一次构建/测试确认。
