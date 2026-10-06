# micro-common · Go 公共库

go-zero 内置能力之外的**补集包**（go-zero 已内置的熔断/限流/单飞/本地缓存等不重复造），被全部 18 个后端服务依赖。**唯一 semver 版本化发布的仓库**——合并 main 自动打 tag（v0.1.x 递增）。

包清单（11 个，自 S1 起逐个落地，接口口径见《02-技术文档》§8）：

| 包 | 一句话职责 |
|---|---|
| errcode | 错误码分段注册器（万位段），gRPC status 无损往返 |
| response | `{code,msg,data}` 信封，基于 `httpx.SetErrorHandler` 全局注册 |
| ctxkit | 操作人/租户/端/角色/data_scope 统一从 context 取写 |
| snowflake | 分布式 ID：etcd 租约分配 worker_id |
| crypto | argon2id / AES-256-GCM + KeyProvider（kid 轮换）/ Mask* 脱敏 |
| jwtauth | RS256 签发校验，header 内置 kid + 多公钥集合 |
| sessionx | Redis DB4 会话中心（互斥/踢人/refresh 轮换/重用墓碑） |
| authz | BFF 三层鉴权中间件（fail-open / fail-close 分级） |
| tenantx | 多租户 SQL 规约支撑包（ADR-08 配套，tenantaudit 把关） |
| eventbus | Outbox + Envelope + Relay（DB 退避重投 ≤16 次 → dead 表）+ kq Pusher 封装 |
| testinfra | 测试环境三级解析（MICRO_TEST_* → 本机 → testcontainers → Skip） |

## 版本化约定

- 合并 main 自动打 tag：`v0.1.x` 递增（CI `tag` 工作流；**有业务 Go 源码才发布**，骨架期不打 tag，首个 tag v0.1.0 随 S1 首包落地）
- 破坏性变更升次版本 + CHANGELOG；服务侧 go.mod `require` + go.work 本地解析（02 §4.2）
- tag 即发布：服务仓库按版本引用，不追 main

## 常用命令

```bash
make test   # go test ./...
make lint   # golangci-lint v2（CI 同款）
make vet    # go vet ./...
```

## 提交与评审约定

- 中文 conventional commits + 任务号：`feat(errcode): S1-01 万位段分段注册`
- PR 走 DoD 自查模板（.github/pull_request_template.md）；全仓 CODEOWNERS 必评审（公共库影响面 = 18 个服务）
