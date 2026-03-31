# Claude 模型性能测试方案

## 1. 测试目标

评估 Claude Opus 4.6 和 Claude Sonnet 4.6 在 Amazon Bedrock 上的推理性能，重点指标：

- **TTFT (Time To First Token)**：首 Token 响应延迟
- **Throughput**：输出吞吐量（tokens/s）

## 2. 测试环境

| 项目 | 说明 |
|------|------|
| 区域 | ap-northeast-1 (Tokyo) |
| 接入方式 | NLB (TCP 直通) → VPC Endpoint → Bedrock Runtime |
| NLB 地址 | `bedrock-nlb-8b69cce82f32c532.elb.ap-northeast-1.amazonaws.com` |
| 协议 | HTTP/2 + TLS 1.3 |
| API | `InvokeModelWithResponseStream` |

### 测试模型

| Inference Profile ID | 模型 |
|---------------------|------|
| `global.anthropic.claude-opus-4-6-v1` | Claude Opus 4.6 |
| `global.anthropic.claude-sonnet-4-6` | Claude Sonnet 4.6 |

> 使用 Global Inference Profile，凭证及 NLB 详情见 `Enviroment.md`。

## 3. HTTP/2 必须开启

### 性能影响

HTTP/2 对 Bedrock 推理性能有显著影响，**测试必须确保 HTTP/2 开启**。

基准对比（max_tokens=256，3 次取平均，同一 NLB 接入）：

| 协议 | Opus TTFT | Opus Throughput | Sonnet TTFT | Sonnet Throughput |
|------|-----------|-----------------|-------------|-------------------|
| HTTP/1.1 | 4130ms | 18.7 t/s | 1323ms | 63.0 t/s |
| **HTTP/2** | **1589ms** | **60.6 t/s** | **1694ms** | **60.5 t/s** |

> Opus 在 HTTP/2 下 TTFT 降低 60%、throughput 提升 3 倍。

### 各 SDK HTTP/2 支持情况

| SDK | HTTP/2 支持 | 方式 |
|-----|------------|------|
| **Go aws-sdk-go-v2** | 原生支持 | TLS 下自动启用 |
| **Python httpx + h2** | 支持 | `httpx.Client(http2=True)` |
| **Java Netty** | 支持 | `.protocol(Protocol.HTTP2)` |
| **Node.js** | 支持 | `NodeHttp2Handler` |
| Python boto3 | 不支持 | 仅 HTTP/1.1，需改用 httpx 方案 |

> 本项目提供了 Python httpx 方案（`test_ttft_h2.py`）和 Go 方案（`go-test/main.go`）的完整示例。

## 4. 测试前 Checklist

| # | 检查项 | 要求 | 原因 |
|---|--------|------|------|
| 1 | HTTP/2 | **必须确认开启** | 见第 3 章性能数据，影响显著 |
| 2 | TLS 1.3 | **确认未降级到 TLS 1.2** | 握手从 2-RTT 降到 1-RTT，服务端已确认支持 |
| 3 | Streaming API | 用 `InvokeModelWithResponseStream` | 才能测到 TTFT |
| 4 | 连接复用 | 保持长连接，不要每次新建 | 省掉 TCP + TLS 握手 |
| 5 | TCP Keepalive | 间隔 < 300s | NLB 空闲超时 350s |
| 6 | 连接预热 | 正式测试前先发预热请求 | 消除冷启动延迟 |
| 7 | Bearer Token | 确认未过期 | ABSK token 有有效期 |

## 5. 核心指标

| 指标 | 含义 | 单位 |
|------|------|------|
| TTFT | 请求发出 → 收到第一个 streaming token | ms |
| 单请求吞吐 | output_tokens / 生成耗时 | tokens/s |
| 系统吞吐 | total_output_tokens / 测试总时长 | tokens/s |

## 6. SDK 配置与示例代码

### Python — httpx + Bearer Token（推荐）

```bash
pip install httpx[http2] botocore
```

完整示例见 `test_ttft_h2.py`，核心要点：
- 使用 `httpx.Client(http2=True)` 建立 HTTP/2 连接
- Bearer Token 认证（`Authorization: Bearer <token>`）
- 用 `botocore.eventstream.EventStreamBuffer` 解析 Bedrock 二进制事件流
- DNS 重写将 Bedrock 域名解析到 NLB IP（等效 curl `--resolve`）

运行方式：
```bash
export AWS_BEARER_TOKEN_BEDROCK=ABSK...
python3 test_ttft_h2.py
```

> **注意：** 不能直接用 `endpoint_url` 指向 NLB 域名，因为 TLS 证书是 Bedrock 域名。必须通过 DNS 重写让客户端以 Bedrock 域名发起连接，实际解析到 NLB IP。

### Go — 原生 HTTP/2（推荐）

完整示例见 `go-test/main.go`，核心要点：
- Go >= 1.21 默认支持 HTTP/2、TLS 1.3、TCP_NODELAY
- 通过自定义 `DialContext` 实现 DNS 重写到 NLB

### SDK 配置速查

| SDK | HTTP/2 | TLS 1.3 | TCP_NODELAY |
|-----|--------|---------|-------------|
| Python httpx | `httpx.Client(http2=True)` + `pip install h2` | 默认 | 默认 |
| Go | 自动（TLS 下） | Go 1.21+ 默认 | 默认 |
| Python boto3 | **不支持** | - | - |

## 7. 执行计划

### 预热

每个场景正式测试前执行 **3 次预热请求**，丢弃结果。

### 采样

每个场景执行 **30 次以上**有效请求，取统计值。

### 执行顺序

1. 单请求基线 — 对 Opus 4.6 和 Sonnet 4.6 分别执行
2. 并发梯度测试
3. **200K 超长上下文 TTFT** — 重点场景
4. 混合负载

## 8. 注意事项

- **HTTP/2 必须确认**：对 TTFT 和 throughput 影响显著，见第 3 章数据
- **DNS 重写**：必须将 Bedrock 域名解析到 NLB IP，直接用 NLB endpoint_url 会导致 TLS SNI 不匹配
- **Token 计数**：使用 API 返回的 `usage` 字段，不要自行分词计数
- **网络基线**：先测量到 NLB 的 RTT 作为基线
- **配额限制**：注意 Bedrock RPM/TPM 配额，避免限流影响结果
- **结果保存**：每次请求原始数据均需保存，便于事后分析
- **环境隔离**：测试期间避免其他程序占用网络带宽
- **Prompt Caching**：重复 system prompt 场景建议开启
