# 发布阻塞项：升级后用户快照无法删除（校验 webhook 误拦）

状态：已修复（代码 + 回归用例），待合入 v1.12.x 发布线
日期：2026-11-18
影响版本：v1.12.0 ~ v1.12.2（所有带 linked-clone 删除守卫、但未带长名字修复的版本）

## 现象

升级到新版本后，删除用户自建快照被校验 webhook 拒绝，报"不满足校验条件"。
快照 CR 永远删不掉（finalizer 卡住，controller 持续重试），只能手改资源对象绕过。

## 根因（是规则变了，不是资源版本变了）

- CRD 版本没有变化：升级前后 Snapshot 均为 `longhorn.io/v1beta2`，不存在存储版本迁移问题。
- 变化的是校验规则：新版本为 linked-clone（链接克隆）特性新增了快照 **Delete** 校验。
  旧版本（v1.11.x）的快照校验器只拦截 Create/Update，DELETE 操作根本不进 webhook；
  新版本把 Delete 加入 `OperationTypes`，且 webhook `failurePolicy: Fail`，
  于是删除路径第一次开始被校验拦截。
- 新守卫 `IsSnapshotLinkedCloneEntrypoint` 用快照名作为 label 值构造选择器
  （`longhorn.io/linked-clone-source-snapshot=<快照名>`）。Kubernetes label 值上限 63 字节，
  而快照名（k8s 对象名）最长可达 253 字节。名字超过 63 字节的快照会让
  `metav1.LabelSelectorAsSelector` 直接报错，Delete 返回 InternalError，删除被拒绝。
- 名字不合法（超过 63 字节）的快照绝不可能是 linked-clone 入口：克隆卷在创建时
  会把原始快照名盖到 label 上，名字存不进 label 的克隆在创建阶段就会被 apiserver 拒绝。
  因此这类快照可以直接放行——这正是修复的逻辑。

复现证据（修复前跑回归用例的实际报错）：

```
failed to check if snapshot backup-snapshot-0000000-pvc-00000000-0000-0000-0000-000000000000
is a linked-clone entrypoint: values[0][longhorn.io/linked-clone-source-snapshot]:
Invalid value: "backup-snapshot-0000000-...": must be no more than 63 bytes
```

## 变更记录（谁改的、为什么）

以下来自 longhorn-manager 提交历史（`git log -- webhook/resources/snapshot/validator.go`）：

| 提交 | 说明 | 作者 | 依据 |
| --- | --- | --- | --- |
| `cde1fa09`（cherry-pick 自 `b0d9850b`） | feat(webhook): block deletion of linked-clone entrypoint snapshots —— 新增 Delete 守卫，保护链接克隆卷的入口快照不被删除（克隆卷与入口快照共享数据，删了克隆即损坏） | Shuo Wu | longhorn/longhorn#12552 |
| `6b1253f0`（cherry-pick 自 `50de0bef`） | fix(webhook): allow snapshot and backup operations on linked-clone volumes —— 放开 Create 误伤，保留 Delete 守卫 | Shuo Wu | longhorn/longhorn#12552 |
| `8458b6e7`（cherry-pick 自 `e5caa63b`） | fix(webhook): reject snapshot/backup/expansion/migration for legacy linked-clone volumes | Shuo Wu | longhorn/longhorn#12552 |
| `bec5e3f8` | fix(snapshot): allow deleting snapshots whose name exceeds 63 characters —— 本次问题的上游修复，目前在 master，未回合 v1.12.x | Derek Su | longhorn/longhorn#14562 |

结论：规则是随 linked-clone 特性（#12552）新增的，意图合理（保护入口快照），
但实现把"名字不是合法 label 值"的普通用户快照一并误伤；上游 #14562 已确认并修复。

## 升级前已存在的快照是否受影响

受影响。webhook 在删除时评估的是当前对象，与创建时间无关：
凡是名字不是合法 label 值（典型为超过 63 字节）的存量快照，只要新版本 webhook 在运行，
一样删不掉。名字不超过 63 字节的存量快照不受影响。

### 批量修复办法

- 该 bug 纯在 webhook 代码里，存量对象数据没有任何损坏，**不需要逐对象迁移**。
- 滚动升级 longhorn-manager（带上本修复）后，所有存量快照的删除立即恢复，
  一次发布即完成全量修复，满足"不能只修新创建的"。
- 应急绕过（修复发布前）：从 `longhorn-webhook-validator`
  ValidatingWebhookConfiguration 的 snapshots 规则里临时摘掉 DELETE 操作。
  注意 longhorn-manager 会在 CA secret 变化时重建该配置，绕过是临时的；
  手改资源对象（删 finalizer）的方式不要再使用。

## 修复内容

代码（longhorn-manager，工作区 `longhorn-manager/` 目录，基于 v1.12.x）：

- `datastore/longhorn.go` `IsSnapshotLinkedCloneEntrypoint`：
  快照名不是合法 label 值时直接返回 `false`（不可能是入口快照），不再构造会报错的选择器。
  与上游修复 `bec5e3f8` 完全一致。补丁文件：`snapshot-delete-validation-fix.patch`。
- 新增回归用例 `webhook/resources/snapshot/validator_test.go`（见下节）。

行为对照：

| 场景 | 修复前 | 修复后 |
| --- | --- | --- |
| 删除普通用户快照（任意合法名字） | 名字超过 63 字节时被误拦 | 放行 |
| 删除 linked-clone 入口快照 | 拦截 | 拦截，报文明确：`cannot delete snapshot X: it is the entrypoint for linked-clone volume(s): clone-a, clone-b` |
| 快照创建、回滚流程 | 不变 | 不变（未触碰 Create/Update 路径与 controller） |
| 其他资源的校验规则 | 不变 | 不变（校验器按资源类型分目录，规则按资源逐条生成，互不影响） |

## 回归用例

`webhook/resources/snapshot/validator_test.go` 的 `TestSnapshotValidatorDelete`：

- 普通快照（无克隆卷引用）删除，断言放行
- linked-clone 入口快照删除，断言拒绝，且报文含快照名与依赖克隆卷名（可读懂的原因）
- 名字恰好 63 字节，断言放行
- 名字 64 字节（备份快照典型命名），断言放行

验证记录：

- 带修复：`go test ./webhook/resources/snapshot/... ./datastore/...` 全部通过；`go test ./webhook/...` 全绿；`go vet` 干净。
- 临时回退修复后：用例如期失败，报错与生产现象一致（`must be no more than 63 bytes`），证明用例守得住该回归。

## 验收步骤（升级后的集群）

1. 建一个名字超过 63 字节的快照（或普通快照），`kubectl delete lhsnap <name>`，断言删除成功、对象不再存在。
2. 制造真正非法的删除：先建 linked-clone 卷（`cloneMode: linkedClone`），再删其入口快照，
   断言仍被拦截，且报文为 `cannot delete snapshot ... it is the entrypoint for linked-clone volume(s): ...`。
3. 抽查一个升级前创建的存量快照（长名字），断言可正常删除。

## 发布检查项（进 checklist）

- [ ] 本修复（`datastore/longhorn.go` + `validator_test.go`）已合入发布分支
- [ ] CI 中 `webhook/resources/snapshot` 与 `datastore` 包测试通过
- [ ] 升级测试集群完成"验收步骤"1~3 并留档
- [ ] 发布说明注明：受影响版本、应急绕过方式、升级即全量修复（含存量快照）
