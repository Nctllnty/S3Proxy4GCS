# S3Proxy HMAC 凭据架构改造：从平台统一密钥到客户端自有密钥

## 1. 问题描述

### 1.1 当前架构

当前 S3Proxy 使用**一对平台统一的 GCS HMAC 密钥**对所有客户端请求进行重签名：

```
客户端 SDK (任意 AK/SK)
    │
    ▼
S3Proxy (Director)
    │  提取请求 → 丢弃客户端签名
    │  用平台统一密钥 PROXY_AWS_ACCESS_KEY_ID / PROXY_AWS_SECRET_ACCESS_KEY 重签
    │
    ▼
GCS XML API (storage.googleapis.com)
    │  验签：看到的永远是同一个 HMAC 身份
    ▼
```

**配置方式：**

- 环境变量 `PROXY_AWS_ACCESS_KEY_ID` / `PROXY_AWS_SECRET_ACCESS_KEY`（或回退到 `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY`）
- K8s 通过 Secret `s3proxy-credentials` 注入
- 所有客户端 SDK（Go V1/V2、Python、Java V1/V2、C++）共用同一对密钥

**代码位置：**

| 文件 | 行号 | 说明 |
|------|------|------|
| `config/settings.go` | L64-65 | `ProxyAccessKey` / `ProxySecretKey` 字段定义 |
| `config/settings.go` | L240-241 | 环境变量加载逻辑 |
| `main.go` | L88-96 | 启动时强制校验密钥非空 |
| `main.go` | L264-336 | Director 中的重签名逻辑 |
| `main.go` | L275-278 | 使用全局密钥构造 `aws.Credentials` |

### 1.2 问题

1. **无客户端身份隔离**：所有请求在 GCS 侧看到的是同一个 Service Account 身份，无法区分来源
2. **无法利用 GCS IAM**：不能基于客户端身份做细粒度的 Bucket/Object 级权限控制
3. **审计不可追溯**：GCS Cloud Audit Logs 中所有操作的 principal 都是同一个 SA
4. **密钥轮换影响全局**：更新密钥需要重启所有 Proxy 实例，且影响所有客户端
5. **违反最小权限原则**：单一高权限 SA 密钥被所有客户端共享

### 1.3 期望目标

每个客户端使用**自己的 GCS HMAC AK/SK**，Proxy 基于客户端凭据重签，使 GCS 侧能识别到真实的客户端身份。

## 2. 技术约束：为什么必须重签名

### 2.1 SigV4 规范强制签名 Host Header

AWS SigV4 规范明确要求：

> "You **must** include the host header (HTTP/1.1) or the :authority header (HTTP/2), and any x-amz-\* headers in the signature."
> — [AWS SigV4 Signing Elements](https://docs.aws.amazon.com/IAM/latest/UserGuide/reference_sigv-create-signed-request.html)

S3 API 进一步要求：

> "The CanonicalHeaders list **must include** the HTTP host header."
> — [S3 SigV4 Header-Based Auth](https://docs.aws.amazon.com/AmazonS3/latest/API/sig-v4-header-based-auth.html)

GCS 的 S3 兼容层完全遵循此规范。

### 2.2 Host 变更导致签名失效

```
1. 客户端签名:  Host = s3proxy-service:8080    → 计算签名 Sig_client
2. Proxy 转发:  Host = storage.googleapis.com   → GCS 期望签名 Sig_gcs
3. Sig_client ≠ Sig_gcs → 403 SignatureDoesNotMatch
```

Proxy 的 Director 必须改写 Host 才能将请求路由到 GCS，而 Host 是签名的组成部分，因此**纯透传不重签在技术上不可行**。

### 2.3 DNS 劫持方案的不可行性

理论上，如果客户端直接配置 `endpoint = https://storage.googleapis.com`，通过 DNS 劫持将域名解析到 Proxy IP，客户端签名的 Host 就是 `storage.googleapis.com`，签名可以直接传递给 GCS。

**但此方案不可行：**

- 需要为 `storage.googleapis.com` 做 TLS 终结（无法获得该域名的合法证书）
- GKE 内需修改 CoreDNS 配置，影响集群其他服务对 GCS 的正常访问
- 完全不透明，排查问题困难
- 与项目"无感迁移"的设计理念冲突

## 3. 解决方案：凭据映射 + 客户端自有密钥重签

### 3.1 架构概览

```
客户端 SDK (自己的 GCS HMAC AK/SK)
    │  Authorization: AWS4-HMAC-SHA256 Credential=<客户端AK>/...
    │
    ▼
S3Proxy (Director)
    │  1. 从 Authorization header 提取客户端 AK
    │  2. 从凭据映射表查找对应 SK
    │  3. 用客户端的 AK/SK 重签名（Host 改为 storage.googleapis.com）
    │
    ▼
GCS XML API
    │  验签：看到的是客户端自己的 HMAC 身份
    │  IAM / Audit Log 可追溯到具体 Service Account
    ▼
```

### 3.2 凭据映射存储设计

#### 方案选型

| 方案 | 热加载 | 运维复杂度 | 适用场景 |
|------|--------|-----------|----------|
| A: K8s Secret → 环境变量 | 需重启 Pod | 低 | 客户端极少、变更不频繁 |
| **B: K8s Secret → Volume 挂载 + fsnotify（推荐）** | ~60s 自动生效 | 低 | 生产环境、多客户端 |
| C: Google Secret Manager + 定时拉取 | 可配置 | 中 | 跨集群、合规要求高 |

#### 推荐方案：K8s Secret Volume 挂载 + fsnotify 文件热加载

**架构流程：**

```
K8s Secret (credentials.json)
    │  kubelet 自动同步变更（~60s）
    ▼
Volume Mount → /etc/s3proxy/credentials.json
    │  fsnotify 监听文件变更事件
    ▼
Proxy 内存 map[AK]SK （atomic.Value 原子替换）
    │  请求到达时 → Load() 获取当前 map → 查找 AK
    ▼
零停机、零锁竞争
```

**K8s Secret 定义：**

```yaml
apiVersion: v1
kind: Secret
metadata:
  name: s3proxy-credentials
type: Opaque
stringData:
  credentials.json: |
    {
      "GOOG1E_CLIENT_A_AK": "secretA",
      "GOOG1E_CLIENT_B_AK": "secretB"
    }
```

**Deployment Volume 挂载：**

```yaml
volumeMounts:
  - name: hmac-credentials
    mountPath: /etc/s3proxy
    readOnly: true
volumes:
  - name: hmac-credentials
    secret:
      secretName: s3proxy-credentials
```

**热加载实现（Go 伪码）：**

```go
var credStore atomic.Value  // 存储 map[string]string

func watchCredentials(path string) {
    watcher, _ := fsnotify.NewWatcher()
    watcher.Add(filepath.Dir(path))  // 监听目录（K8s 用 symlink 更新）
    for event := range watcher.Events {
        if event.Op&(fsnotify.Create|fsnotify.Write) != 0 {
            data, _ := os.ReadFile(path)
            var m map[string]string
            json.Unmarshal(data, &m)
            credStore.Store(m)  // 原子替换，无锁
            slog.Info("Credentials reloaded", "count", len(m))
        }
    }
}

func getSecretKey(ak string) (string, bool) {
    m := credStore.Load().(map[string]string)
    sk, ok := m[ak]
    return sk, ok
}
```

**关键设计点：**

- 使用 `atomic.Value` 而非 `sync.RWMutex`，读路径零锁竞争，不影响数据面性能
- 监听目录而非文件：K8s 通过 symlink 原子更新 Secret 文件，直接 watch 文件会丢失事件
- kubelet 同步延迟约 60s（可通过 kubelet `--sync-frequency` 调整）

#### 向后兼容：环境变量方式

适合客户端极少的简单场景，或本地开发：

```bash
# JSON 格式：AK → SK 映射
HMAC_CREDENTIALS='{"GOOG1E_CLIENT_A_AK":"secretA","GOOG1E_CLIENT_B_AK":"secretB"}'
```

如果设置了旧的 `PROXY_AWS_ACCESS_KEY_ID` / `PROXY_AWS_SECRET_ACCESS_KEY`，自动转为单条映射条目，不影响现有部署。

### 3.3 AK 提取逻辑

SigV4 Authorization header 格式固定：

```
Authorization: AWS4-HMAC-SHA256 Credential=<AccessKeyID>/<date>/<region>/<service>/aws4_request, SignedHeaders=..., Signature=...
```

提取 AK 的逻辑：

```go
func extractAccessKey(authHeader string) (string, error) {
    // 解析 "Credential=<AK>/..." 部分
    idx := strings.Index(authHeader, "Credential=")
    if idx < 0 {
        return "", fmt.Errorf("no Credential field in Authorization header")
    }
    rest := authHeader[idx+len("Credential="):]
    slashIdx := strings.Index(rest, "/")
    if slashIdx < 0 {
        return "", fmt.Errorf("malformed Credential field")
    }
    return rest[:slashIdx], nil
}
```

### 3.4 重签名流程变更

**当前**（main.go L275-278）：

```go
awsCreds := aws.Credentials{
    AccessKeyID:     config.Config.ProxyAccessKey,     // 全局唯一
    SecretAccessKey: config.Config.ProxySecretKey,      // 全局唯一
}
```

**改造后**：

```go
clientAK, err := extractAccessKey(req.Header.Get("Authorization"))
if err != nil {
    slog.Error("Failed to extract AK from Authorization header", "error", err)
    // 返回 403
    return
}
clientSK, ok := config.Config.HMACCredentials[clientAK]
if !ok {
    slog.Warn("Unknown client AK, not in credential mapping", "ak", clientAK)
    // 返回 403 InvalidAccessKeyId
    return
}
awsCreds := aws.Credentials{
    AccessKeyID:     clientAK,
    SecretAccessKey: clientSK,
}
```

### 3.5 错误处理

对于未知 AK（不在映射表中的客户端），返回标准 S3 XML 错误：

```xml
<Error>
  <Code>InvalidAccessKeyId</Code>
  <Message>The AWS Access Key Id you provided does not exist in our records.</Message>
  <AWSAccessKeyId>GOOG1E_UNKNOWN_AK</AWSAccessKeyId>
</Error>
```

### 3.6 多客户端支持

同一个 Proxy 实例可同时服务多个客户端，每个请求独立路由：

```
客户端 A (AK_A/SK_A) ──┐
                        ├──▶ S3Proxy ──▶ 提取 AK → 查映射表 → 用对应 SK 重签 ──▶ GCS
客户端 B (AK_B/SK_B) ──┘
```

- 映射表支持任意数量条目
- 每个客户端的 GCS 身份独立，IAM 权限互不干扰
- Cloud Audit Logs 可追溯到具体 Service Account

### 3.7 SK 不可自动发现（密码学约束）

SigV4 请求中 **AK 明文可见，SK 永远不出现在网络上**。Proxy 无法从请求中反推 SK（等同于暴力破解 HMAC-SHA256）。GCP `hmacKeys.get` API 也不返回 SK（SK 仅在创建时返回一次）。

因此映射表必须通过外部配置提供，不能自动学习。

### 3.8 一键运维脚本

将 HMAC 密钥创建 + 映射表注册合为一步，最大程度减少手工操作：

```bash
#!/bin/bash
# create-client-hmac.sh — 创建 GCS HMAC 密钥并注册到 Proxy 映射表

SA_EMAIL=$1  # 客户端 Service Account

# 1. 创建 HMAC Key（GCP 返回 AK + SK）
RESULT=$(gcloud storage hmac create "$SA_EMAIL" --format=json)
AK=$(echo "$RESULT" | jq -r '.metadata.accessId')
SK=$(echo "$RESULT" | jq -r '.secret')

# 2. 读取现有映射 → 追加新条目 → 更新 K8s Secret
EXISTING=$(kubectl get secret s3proxy-credentials -o jsonpath='{.data.credentials\.json}' | base64 -d)
UPDATED=$(echo "$EXISTING" | jq --arg ak "$AK" --arg sk "$SK" '. + {($ak): $sk}')

kubectl create secret generic s3proxy-credentials \
  --from-literal=credentials.json="$UPDATED" \
  --dry-run=client -o yaml | kubectl apply -f -

# 3. Proxy 通过文件监听自动重载（~60s），无需重启
echo "Done. AK=$AK"
echo "请将以下凭据交给客户端配置："
echo "  aws_access_key_id=$AK"
echo "  aws_secret_access_key=$SK"
```

运维只需一条命令，Proxy 自动热加载生效：

```bash
./create-client-hmac.sh sa-team-a@project.iam.gserviceaccount.com
```

## 4. 改造范围

### 4.1 代理核心

| 文件 | 改动 |
|------|------|
| `config/settings.go` | 新增 `HMACCredentials map[string]string` + `CredentialsFile string` 字段；新增 `HMAC_CREDENTIALS_FILE` 环境变量支持；保留旧变量向后兼容 |
| `main.go` | 新增 `extractAccessKey()` 函数；Director 中提取客户端 AK → 查映射表 → 用客户端凭据重签；新增 `watchCredentials()` 热加载；启动校验逻辑适配 |
| `.env` | 新增 `HMAC_CREDENTIALS` / `HMAC_CREDENTIALS_FILE` 配置项及说明，保留旧变量兼容 |

### 4.2 K8s 部署

| 文件 | 改动 |
|------|------|
| `k8s/deployment.yaml` | Secret 从环境变量注入改为 Volume 挂载到 `/etc/s3proxy/credentials.json` |
| `.github/workflows/benchmark.yml` | Secret 创建命令适配 `credentials.json` JSON 格式 |
| `.github/workflows/multi-sdk-e2e-tests.yml` | 同上 |
| `.github/workflows/direct-benchmark.yml` | 同上（如需要） |

### 4.3 测试适配

所有 SDK 测试已经通过 `GCS_HMAC_ACCESS` / `GCS_HMAC_SECRET` 环境变量配置客户端凭据，**测试代码本身不需要改动**，只需确保：

1. Proxy 的凭据映射表中包含测试用的 AK/SK 条目
2. CI workflow 中 Secret 创建命令适配新格式

| 测试套件 | 凭据加载方式 | 是否需要改动 |
|----------|-------------|-------------|
| `sdk_tests/go-v2` | `GCS_HMAC_ACCESS` / `GCS_HMAC_SECRET` | 无需改动 |
| `sdk_tests/go-v1` | 同上 | 无需改动 |
| `sdk_tests/python` | 同上 | 无需改动 |
| `sdk_tests/java-v1` | 同上 | 无需改动 |
| `sdk_tests/java-v2` | 同上 | 无需改动 |
| `sdk_tests/cpp` | 同上 | 无需改动 |
| `e2e_tests` | 同上 | 无需改动 |
| `integration_tests` | `AWS_ACCESS_KEY_ID` / `.env` 回退 | 需适配新 `.env` 格式 |

### 4.4 文档更新

| 文件 | 改动 |
|------|------|
| `.env` | 第 2 节"S3 重签名凭据"改为新格式，保留旧变量兼容说明 |
| `AGENTS.md` | 更新密钥管理相关描述 |
| `README.md` | 更新部署配置说明 |

## 5. 向后兼容策略

```
优先级：HMAC_CREDENTIALS > PROXY_AWS_ACCESS_KEY_ID + PROXY_AWS_SECRET_ACCESS_KEY > AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY

if HMAC_CREDENTIALS 已设置:
    解析 JSON → map[AK]SK
else if PROXY_AWS_ACCESS_KEY_ID 已设置:
    map = {PROXY_AWS_ACCESS_KEY_ID: PROXY_AWS_SECRET_ACCESS_KEY}
    slog.Warn("使用旧版单密钥配置，建议迁移到 HMAC_CREDENTIALS")
else if AWS_ACCESS_KEY_ID 已设置:
    map = {AWS_ACCESS_KEY_ID: AWS_SECRET_ACCESS_KEY}
    slog.Warn("使用旧版单密钥配置，建议迁移到 HMAC_CREDENTIALS")
else:
    log.Fatal (DryRun=false 时)
```

现有部署无需任何改动即可继续运行，只是会看到一条迁移建议日志。

## 6. 实施状态（v1.7 开发中）

- [x] 设计评审确认
- [x] `pkg/credstore` 新包：`atomic.Value` 存储 + `fsnotify` 热加载 + AK 提取工具
- [x] `pkg/metrics` 新增 3 个指标：`s3proxy_hmac_credential_lookups_total{result}`、
      `s3proxy_hmac_credentials_loaded`、`s3proxy_hmac_credentials_reload_total{result}`
- [x] `config/settings.go` 凭据映射加载（`HMAC_CREDENTIALS_FILE` / `HMAC_CREDENTIALS` /
      旧版单密钥回退），`HMACStrict` 开关
- [x] `main.go` AK 提取（`Authorization` header + `X-Amz-Credential` query 兜底）+
      映射查找 + 通过 `context.WithValue` 把 `aws.Credentials` 传给 Director 重签
- [x] `main.go` 启动时调用 `credstore.Watch` 订阅文件变更（K8s Secret symlink 也支持）
- [x] 单元测试：
      - `pkg/credstore/credstore_test.go` — AK 解析、Replace 防御性拷贝、ParseJSON 严格校验、
        `LoadFile`、`Watch` 的文件改写 + 脏数据回滚
      - `credential_test.go` — `validateClientCredential` 的 disabled/hit/miss/no-auth/
        presigned 路径 + `credentialsFromContext` nil 兜底
- [x] `k8s/deployment.yaml`：以 `s3proxy-hmac-credentials` Secret Volume 挂载到
      `/etc/s3proxy/credentials.json`，配套 `HMAC_CREDENTIALS_FILE`、`HMAC_STRICT=true`
- [x] CI workflow 适配（`multi-sdk-e2e-tests.yml` / `benchmark.yml` / `direct-benchmark.yml`），
      旧的 `s3proxy-credentials` Secret 已被替换
- [x] 运维脚本 `scripts/create-client-hmac.sh`（创建 HMAC + 合并进 Secret + 热加载验证提示）
- [x] 文档更新（README.md / AGENTS.md §15 / `.env` / 本文件实施状态）
- [ ] 端到端验证（待 CI 跑一次 `multi-sdk-e2e-tests` 确认重签通过）

### 6.1 仓库改动概览

| 文件/目录 | 改动 |
|-----------|------|
| `pkg/credstore/credstore.go` | 新建：AK→SK 映射、`atomic.Value` 无锁读、fsnotify 热加载、防御性拷贝、AK 提取（header + query）、输入严格校验 |
| `pkg/credstore/credstore_test.go` | 新建：上述能力的表驱动测试 + hot-reload 回滚场景 |
| `pkg/metrics/metrics.go` | 新增 3 个 HMAC 相关指标 |
| `config/settings.go` | 新增 `HMACCredentials`、`CredentialsFile`、`HMACStrict` 字段与加载逻辑 |
| `main.go` | 全局 `hmacCredentials` 存储、启动时 `Replace`+`Watch`、`validateClientCredential`、`credentialsFromContext`、Director 从 ctx 取凭据 |
| `credential_test.go` | 新建：`validateClientCredential` 所有分支的黑盒测试 |
| `k8s/deployment.yaml` | 删除 `PROXY_AWS_*` env，新增 Secret Volume 挂载 + `HMAC_CREDENTIALS_FILE` |
| `.github/workflows/*.yml` | 用 `s3proxy-hmac-credentials` Secret 取代 `s3proxy-credentials` |
| `scripts/create-client-hmac.sh` | 新增：客户端 HMAC 创建 + 映射合并 + kubectl apply 一条龙 |
| `README.md` / `AGENTS.md` / `.env` | 文档同步到新凭据模型 |
