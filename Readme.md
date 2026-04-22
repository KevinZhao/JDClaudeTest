# Claude 模型性能测试方案（直连 NLB）

## 1. 测试目标

评估 Claude 系列模型在 Amazon Bedrock 上的推理性能，所有集成测试统一走 **直连 NLB** 路径，重点指标：

- **TTFT (Time To First Token)**：首 Token 响应延迟
- **Throughput**：输出吞吐量（tokens/s）
- **TPM**：每分钟 token 吞吐（负载场景下的系统上限）

## 2. 测试环境

| 项目 | 说明 |
|------|------|
| 区域 | `ap-northeast-1` (Tokyo) |
| 接入方式 | **NLB (TCP 443 直通) → VPC Endpoint → Bedrock Runtime** |
| NLB 地址 | `bedrock-nlb-8b69cce82f32c532.elb.ap-northeast-1.amazonaws.com` |
| NLB 监听 | TCP:443（不做 TLS 终结） |
| Cross-Zone LB | 关闭（客户端 AZ 亲和） |
| VPC Endpoint | `vpce-0af0abeac0154e7c1`（Interface，Bedrock Runtime） |
| NLB ENI | AZ-1a: `10.0.1.192` / AZ-1c: `10.0.2.145` |
| 协议 | HTTP/2 + TLS 1.3（端到端一次握手） |
| API | `InvokeModelWithResponseStream` |
| 认证 | Bearer Token 或 SigV4（AKSK） |

> **凭证** (`AWS_BEARER_TOKEN_BEDROCK` / AKSK) 仅通过环境变量注入，**不写入仓库**。实际值参见本地 `Enviroment.md`（已 gitignore）。

### 测试模型（Global Inference Profile）

| Inference Profile ID | 模型 |
|---------------------|------|
| `global.anthropic.claude-opus-4-7` | Claude Opus 4.7 |
| `global.anthropic.claude-opus-4-6-v1` | Claude Opus 4.6 |
| `global.anthropic.claude-sonnet-4-6` | Claude Sonnet 4.6 |

> Opus 4.7 的 model ID 无 `-v1` 后缀；使用前需确认账户已开启该模型访问。

## 3. 直连 NLB 的关键技术点

### 3.1 TLS SNI 与 DNS 重写

NLB 做 TCP 直通，不终结 TLS。Bedrock Runtime 的证书只绑定 `bedrock-runtime.<region>.amazonaws.com`，因此：

- **不能** 直接把 SDK 的 `endpoint_url` 指向 NLB 域名 → 会出 TLS SNI / 证书 CN 不匹配
- **必须** 在客户端做 DNS 重写：`bedrock-runtime.<region>.amazonaws.com` → NLB IP
- 客户端仍以 Bedrock 域名发起 TLS 握手，SNI 正确，证书校验通过

### 3.2 HTTP/2 必须开启

HTTP/2 对 TTFT 和 throughput 影响显著。同一 NLB 接入、3 次取均值的基线对比：

| 协议 | Opus TTFT | Opus Throughput | Sonnet TTFT | Sonnet Throughput |
|------|-----------|-----------------|-------------|-------------------|
| HTTP/1.1 | 4130 ms | 18.7 t/s | 1323 ms | 63.0 t/s |
| **HTTP/2** | **1589 ms** | **60.6 t/s** | **1694 ms** | **60.5 t/s** |

各 SDK 的 HTTP/2 启用方式：

| SDK | HTTP/2 支持 | 启用方式 |
|-----|------------|----------|
| Go aws-sdk-go-v2 | 原生 | TLS 下自动；`ForceAttemptHTTP2: true` + `http2.ConfigureTransport` |
| Python httpx + h2 | 支持 | `httpx.Client(http2=True)` |
| Python boto3 | **不支持** | 仅 HTTP/1.1，必须换实现 |

## 4. 测试前 Checklist

| # | 检查项 | 要求 | 原因 |
|---|--------|------|------|
| 1 | HTTP/2 | 必须确认实际已启用（抓包或看 `ALPN=h2`） | 见第 3.2 节性能数据 |
| 2 | TLS 1.3 | 确认未降级 | 握手从 2-RTT 降到 1-RTT |
| 3 | DNS 重写 | 确认目标 IP 为 NLB，而非 Bedrock 公网 | 证书仍是 Bedrock 域名 |
| 4 | Streaming API | 使用 `InvokeModelWithResponseStream` | 才能测到 TTFT |
| 5 | 连接复用 | 保持长连接，不要每请求新建 | 省掉 TCP + TLS 握手 |
| 6 | TCP Keepalive | 间隔 < 300 s | NLB 空闲超时 350 s |
| 7 | 连接预热 | 正式测试前至少 1 次 full-payload warmup | 消除冷启动 |
| 8 | 凭证有效 | Bearer Token 未过期 / AKSK 有权限 | Token 有有效期 |
| 9 | 配额 | 留意 Bedrock RPM/TPM 配额 | 避免限流污染结果 |

## 5. 核心指标定义

| 指标 | 含义 | 单位 |
|------|------|------|
| TTFT | 请求发出 → 第一个 streaming token 到达 | ms |
| E2E | 请求发出 → 最后一个 token | ms |
| 单请求吞吐 | `output_tokens / (E2E − TTFT)` | tokens/s |
| 系统吞吐 (TPM) | `total_tokens / 测试总时长(min)` | tokens/min |
| 限流率 | `429 / ThrottlingException` 占比 | % |

## 6. 项目中的测试脚本

| 脚本 | 语言 | 用途 |
|------|------|------|
| `test_ttft_h2.py` | Python (httpx + h2) | 单请求 TTFT / 吞吐基线，Bearer Token 认证 |
| `go-test/main.go` | Go (aws-sdk-go-v2) | 单请求 TTFT / 吞吐基线，SigV4 认证 |
| `go-test/loadtest/main.go` | Go | QPS 压测、分钟级 TPM 分析、限流统计 |

三个脚本均在客户端内做 DNS 重写到 NLB，不依赖外部代理。

### 6.1 Python 单请求基线

```bash
pip install 'httpx[http2]' botocore
export AWS_BEARER_TOKEN_BEDROCK=<bearer-token>
python3 test_ttft_h2.py
```

实现要点：
- `httpx.Client(http2=True)` 建立 HTTP/2 连接
- 通过 monkey-patch `socket.getaddrinfo`，把 Bedrock 域名解析到 NLB IP
- 使用 `botocore.eventstream.EventStreamBuffer` 解析 Bedrock 二进制事件流

### 6.2 Go 单请求基线

```bash
cd go-test
# 使用默认 AWS 凭证链（AKSK 或 IAM Role）
go run main.go
```

实现要点：
- 自定义 `http.Transport.DialContext` 做 DNS 重写
- `TLSClientConfig.MinVersion = tls.VersionTLS13`
- `ForceAttemptHTTP2: true` + `http2.ConfigureTransport`
- 通过 `config.WithHTTPClient` 注入到 `aws-sdk-go-v2`

### 6.3 Go 负载压测

```bash
cd go-test/loadtest
go build -o loadtest_bin .

./loadtest_bin \
  -qps=0.5 \
  -duration=5m \
  -model=global.anthropic.claude-opus-4-7 \
  -input-tokens=60000 \
  -max-tokens=1024 \
  -tpm-target=3000000
```

关键参数：

| 参数 | 默认 | 说明 |
|------|------|------|
| `-qps` | `0.5` | 每秒请求数 |
| `-duration` | `5m` | 压测时长 |
| `-model` | `global.anthropic.claude-opus-4-7` | Bedrock 模型 ID |
| `-input-tokens` | `60000` | 目标输入 token 数（生成对应长度 prompt） |
| `-max-tokens` | `1024` | 输出上限 |
| `-tpm-target` | `3000000` | TPM 目标线（用于 PASS/FAIL 判定） |

输出：
- 每次请求一行：TTFT / E2E / in / out / tps
- 每 30 s：滚动 TPM 状态
- 结束后：分钟级直方图、P50/P90/P99、限流率、TPM verdict

## 7. 网络架构（直连路径）

```
┌────────────┐  TLS 1.3 / HTTP/2   ┌──────────────┐   ┌───────────────┐   ┌──────────────────┐
│   Client   │ ──────────────────→ │     NLB      │ → │ VPC Endpoint  │ → │ Bedrock Runtime  │
│ (test app) │   DNS 重写 SNI       │ TCP 443 直通 │   │ (Interface)   │   │ (Inference)      │
└────────────┘                      └──────────────┘   └───────────────┘   └──────────────────┘
                 一次 TLS 1.3 握手，端到端复用
```

```
NLB ─┬── AZ ap-northeast-1a: ENI 10.0.1.192 ──┐
     └── AZ ap-northeast-1c: ENI 10.0.2.145 ──┴──→ VPCE vpce-0af0abeac0154e7c1 → Bedrock Runtime
```

- NLB **不做** TLS 终结，也不做 HTTP 层处理
- Cross-Zone Load Balancing 已关闭（客户端 AZ 亲和，降低跨 AZ 延迟）
- NLB 空闲超时：350 s（客户端 TCP Keepalive 需 < 300 s）

## 8. 执行计划

### 8.1 预热

每个场景正式测试前执行 **1–3 次 full-payload 预热请求**，结果丢弃。

### 8.2 采样

每个场景至少 **30 次有效请求**，取 avg / P50 / P90 / P99。

### 8.3 推荐执行顺序

1. **单请求基线**：Opus 4.6 / Sonnet 4.6 各跑 `test_ttft_h2.py` 或 `go-test/main.go`
2. **并发梯度**：`loadtest` 从低 QPS 起递增，观察 TTFT / 限流率拐点
3. **200 K 超长上下文 TTFT**：`-input-tokens=200000`，单请求测
4. **长时 TPM 压测**：目标 TPM × 30 min+，观察稳定性
5. **混合负载**（可选）：Opus + Sonnet 并行

## 9. 注意事项

- **凭证不进仓库**：`AWS_BEARER_TOKEN_BEDROCK` 或 AKSK 仅通过环境变量注入
- **HTTP/2 必须确认生效**：不要假设默认启用，建议抓包验证 `ALPN=h2`
- **DNS 重写必须**：直接用 NLB hostname 作为 endpoint 会导致 TLS 握手失败
- **Token 计数**：以 API 返回的 `usage` 字段为准，不自行分词
- **网络基线**：测试前先 `ping` / `tcping` NLB 记录 RTT
- **环境隔离**：压测期间避免共享主机运行其他高带宽任务
- **结果留痕**：原始逐请求指标保存为 CSV/Parquet，便于事后统计
