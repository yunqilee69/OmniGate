# OmniGate 语音（TTS / STT）接入设计计划

> 状态：已完成
> 范围：代理转发面 + 数据层 + 运行层 + 管理 API + 前端配置面
> 不含：Playground 语音 tab（后续独立增量）

---

## 1. 目标

对下游暴露两个 OpenAI 兼容音频端点，接入 OmniGate 既有的三级路由（逻辑路由 → 加权选模型 → 密钥轮询）、熔断、重试、统计与虚拟密钥体系：

| 能力 | 网关端点 | 上游端点 |
|---|---|---|
| 语音合成 TTS | `POST /v1/audio/speech` | `{base}/audio/speech` |
| 语音识别 STT | `POST /v1/audio/transcriptions` | `{base}/audio/transcriptions` |

非目标：不做跨厂商协议转换（与 embeddings/rerank/images 同方针），不代理 Realtime WebSocket API，不做 `/v1/audio/translations`（字段为 STT 子集，留待后续复用同族能力）。

---

## 2. 外部格式事实

依据 openai-python / openai-node / openai-go 三份官方 SDK 生成源码，以及 OpenRouter、Groq 文档核实。

### 2.1 TTS `POST /v1/audio/speech`

请求 JSON：

| 字段 | 必填 | 值域 |
|---|---|---|
| `model` | 是 | `tts-1` `tts-1-hd` `gpt-4o-mini-tts` `gpt-4o-mini-tts-2025-12-15`（兼容端点常扩展自有模型名） |
| `input` | 是 | 文本，上限 4096 字符 |
| `voice` | 是 | string 或对象 `{"id":"voice_1234"}`；内置 `alloy ash ballad coral echo fable onyx nova sage shimmer verse marin cedar` |
| `response_format` | 否 | `mp3`(默认) `opus` `aac` `flac` `wav` `pcm` |
| `speed` | 否 | 0.25–4.0，默认 1.0 |
| `instructions` | 否 | `tts-1`/`tts-1-hd` 不支持 |
| `stream_format` | 否 | `sse` / `audio`；`sse` 不被 `tts-1`/`tts-1-hd` 支持 |

响应：
- 非流式：body 即音频字节，`Content-Type` 为 `audio/mpeg` 等；SDK 强制 `Accept: application/octet-stream`。
- `stream_format:"sse"`：`text/event-stream`，帧形如
  `data: {"type":"speech.audio.delta","audio":"<base64>","sample_rate":24000}`
  `data: {"type":"speech.audio.done","usage":{...}}`

### 2.2 STT `POST /v1/audio/transcriptions`

请求 `multipart/form-data`（数组字段以 `key[]` 重复项传）：

| 字段 | 必填 | 说明 |
|---|---|---|
| `file` | 是 | 音频，格式 `flac mp3 mp4 mpeg mpga m4a ogg wav webm`；需带文件名扩展/Content-Type 供上游识别 |
| `model` | 是 | `whisper-1` `gpt-4o-transcribe` `gpt-4o-mini-transcribe` `gpt-transcribe` `gpt-4o-transcribe-diarize` |
| `language` | 否 | ISO-639-1 |
| `prompt` | 否 | 风格/续写提示 |
| `response_format` | 否 | `json`(默认) `text` `srt` `verbose_json` `vtt` `diarized_json` |
| `temperature` | 否 | 0–1 |
| `timestamp_granularities[]` | 否 | `word` / `segment`，要求 `verbose_json` |
| `stream` | 否 | true 走 SSE（`whisper-1` 忽略） |
| `chunking_strategy` | 否 | `"auto"` 或 `{"type":"server_vad",...}` |
| `include[]` | 否 | `logprobs` |
| `keywords[]` `languages[]` | 否 | 仅 `gpt-transcribe` |
| `known_speaker_names[]` `known_speaker_references[]` | 否 | 仅 diarize |

响应形态：
- `json` → `{"text":"..."}`
- `verbose_json` → `{"text","language","duration","segments":[...],"words":[...],"usage":{"type":"duration","seconds":N}}`
- `text`/`srt`/`vtt` → 纯文本，各 MIME
- `stream:true` → SSE `transcript.text.delta` / `transcript.text.done`（done 带 `usage:{type:"tokens",input_tokens,output_tokens,...,input_token_details:{audio_tokens,text_tokens}}`）

### 2.3 决定实现形态的两个事实

1. **`file` 是 OpenAI SDK multipart 的第一个 part，`model` 在其后** → 目标 URL 依赖 `model`，无法纯流式转发，STT 请求侧必须缓冲。
2. **TTS 响应是二进制/SSE 流，不是 JSON** → 不能沿用 `typedAttempt` 的 `io.ReadAll` 全缓冲（会让首字节延迟等于整个文件时长、并彻底破坏 SSE）。

两者在缓冲方向上互补：

| | 入站 | 出站 |
|---|---|---|
| STT | multipart，大（≤25MB），**需缓冲** | 小 JSON/文本，缓冲即可 |
| TTS | 小 JSON（≤4096 字符），缓冲 | 二进制/SSE，**需流式透传** |

---

## 3. 为什么不能复用 `serveTyped`

`internal/proxy/embeddings.go` 的三处硬阻塞：

| 位置 | 现状 | 音频为何不行 |
|---|---|---|
| `serveTyped` 入口 | `io.ReadAll` + `json.Unmarshal(body)` | STT 入站是 multipart |
| `typedAttempt` | 出站固定 `Content-Type: application/json` | 无法转发 multipart |
| `typedAttempt` 成功分支 | `io.ReadAll(resp.Body)` 全缓冲 | 破坏 TTS 流式；且 `parseUsage` 无从下手 |

另：`typedAttempt` 忽略 `model.ApiPath`（只有 chat adapter 用）。音频兼容端点的上游路径并不统一，本族必须支持路径覆盖。

结论：新增平行实现 `internal/proxy/audio.go`，复用 `serveTyped` 的**控制流骨架**（pendingLog → `PickTyped` → 重试环 → `record` → `writeLog` → `maybeCapture` → fallback），只替换 attempt 内部。

---

## 4. 数据层改动（`internal/store/`）

### 4.1 `models.go`

```go
// Model 新增
AudioSecPrice float64 // billing_mode=audio_second：每音频秒单价
CharPrice     float64 // billing_mode=char：每 1M 字符单价（与 token 口径对齐，避免极小数值）

// RequestLog / RequestLogDaily 新增
AudioSeconds float64 // STT 音频时长（秒）
InputChars   int     // TTS 输入字符数
```

值域扩展：`Model.Type` += `tts` `stt`；`BillingMode` += `audio_second` `char`。

### 4.2 `endpoint.go`

```go
"audio": {"tts", "stt"},
```
`EndpointsForFilter` 自动支持按"语音"族过滤，`Logs.tsx`/`Stats.tsx` 零改动。

### 4.3 `rollup.go`

`UpsertDaily`（实时）与 `Backfill`（启动一次性）两处 SQL 同步补 `audio_seconds`、`input_chars`：INSERT 列、`ON CONFLICT DO UPDATE` 累加、GROUP BY SELECT。

### 4.4 迁移

四张表均已在 `store.go` 的 AutoMigrate 列表内，加列自动完成。`ContentLog` 走手工迁移且本次不加列。

---

## 5. 计量设计（核心）

### 5.1 usageInfo 扩展

`prompt`/`completion`/`cached`/`cacheWrite` 全是 token 语义，**绝不能把秒数塞进 `PromptTokens`** —— 会污染 token 统计口径、TPS 计算与费用聚合。新增独立字段：

```go
seconds float64 // STT 音频时长
chars   int     // TTS 输入字符（精确值，非估算）
```

音频请求不做 token 估算：`PromptTokens`/`CompletionTokens` 保持 0。`streamTPS` 因 `completion<=0` 天然返回 0，无需改动。

### 5.2 音频秒数来源（优先级）

1. **上游 usage**：`verbose_json` 的 `usage.seconds`；SSE `transcript.text.done` 的 token usage（`gpt-4o-transcribe` 系按 token 计费时走这条）。
2. **本地容器头探测**：STT 音频字节已全缓冲在内存，直接解析，零外部依赖（`audiolen.go`）：

   | 容器 | 算法 |
   |---|---|
   | WAV | `fmt` chunk（sample_rate/channels/bits）+ `data` chunk 字节数 → 秒 |
   | MP3 | 帧头解 version/layer/bitrate/samplerate；含 Xing/Info VBRTag 则 `frames × 1152 / sample_rate`，否则按 CBR 码率反推 |
   | FLAC | `STREAMINFO` block 的 `total_samples`(36bit) / `sample_rate`(20bit) |
   | M4A/MP4 | `moov/mvhd` box 的 `duration / timescale`（含 version-1 64bit 分支） |
   | Ogg/Opus | 末页 `granulepos / 48000` |
   | 未覆盖 | 返回 `(0, false)` |

3. **都拿不到** → 量化失败，不计入 `audio_second` 费用。

即：无论上游是否配合返回 usage，只要容器格式在支持列表内就能量化。原设想过的"强制注入 `verbose_json` 换 usage"开关已废弃——它会引入响应重写并破坏协议严格性，且部分模型直接 400 拒绝该格式。

### 5.3 计费三级模式（`cost()` 分支）

```
audio_second → seconds × AudioSecPrice            // STT 主模式
char         → chars   × CharPrice  / 1e6          // TTS 主模式（字符数必然精确可知）
per_call     → 成功一次记 PerCallPrice              // 现成分支；量化失败或厂商按次定价的兜底
```

TTS 因输入字符数恒定精确，实际不需要兜底；只有 STT 时长解析失败时才落到 `per_call`。

CNY→USD 折算、`status != "success"` 不计费等既有约束全部沿用。

---

## 6. 转发实现（`internal/proxy/`）

### 6.1 `audio.go`

```go
type audioKind struct {
    modelType string // "tts" | "stt"，同时作为 request_log.endpoint
    path      string // "/audio/speech" | "/audio/transcriptions"
    isSTT     bool   // 决定 attempt 分支
}
```

**TTS attempt（JSON 入 / 流式出）**
- 入站：限长读 → `json.Unmarshal` → 校验 `model` `input` `voice` → 重写 `model` 为物理名 → 合并 `body_override` → 重新 marshal。
- 出站：`Content-Type: application/json`、`Accept: application/octet-stream`、`Authorization: Bearer` + `ApplyUpstreamIdentity`；URL 支持 `ApiPath` 覆盖。
- 响应按 `Content-Type` 三分支：
  1. `text/event-stream` → 逐帧透传 + `Flush()`。**不对每帧 unmarshal**（帧含几十 KB base64，解析纯属浪费），仅在帧体命中 `"speech.audio.done"` 时解析那一帧取 usage。
  2. `audio/*` / `application/octet-stream` → 流式 copy + `Flush()`。
  3. `application/json` → 上游以 200 返业务错误（兼容端点常见），缓冲后按错误处理。
- 首字节写出即 `committed=true`，此后不可重试（沿用 `streamAttempt` 契约），断流记 `errStreamBroken`。

**STT attempt（multipart 入 / 缓冲出）**
- 入站：全缓冲到 `audio.max_upload_mb`（超限 413）→ `multipart.Reader` 逐 part 解析为 `{Header, Bytes}` → 替换 `model` part 值 → `CreatePart(h.Clone())` 重编码。
  **part 顺序与 header 原样保留**：`filename` 是上游识别音频格式的依据，严格解析器还可能校验顺序。
- 出站：`Content-Type: multipart/form-data; boundary=…`（取 `multipart.Writer.Boundary()`）。
- 响应：缓冲，`Content-Type` 与 body 原样回写；usage 走上游字段，缺失则本地探测。

### 6.2 内容捕获保护

`captureWriter` 缓冲前 1MB 响应体转 string 落 `content_log.response_body` —— 对 TTS 会把 1MB 二进制塞进 SQLite。音频路径改为：
- 响应侧不套 `captureWriter`，`response_body` 记占位摘要 `[binary audio response: N bytes, content-type: audio/mpeg]`。
- STT 请求侧 `client_request_body` 只记字段值，`file` part 替换为 `[audio file: <filename>, N bytes]`，避免 base64 噪声。
- 同步更新 `ContentLog` 注释。

### 6.3 错误码

复用 `errConnectionFailed` / `errTimeout` / `errReadFailed` / `errStreamBroken` / `errAllRetriesFailed` 等。新增 `errAudioTooLarge`（上传超限）。

### 6.4 `probe.go` 模型测试

- TTS：`{"model":name,"input":"ping","voice":<取 body_override.voice，回退 "alloy">}`，成功判据 2xx 且响应字节 > 0。
- STT：内嵌极小合法静音 WAV（硬编码字节）构造 multipart，判据同上。
- 无 `voice` 的 TTS 模型返回 400 时，提示"音频模型测试需在 body_override 配 voice"。

---

## 7. 管理 API（`internal/api/`）

| 位置 | 改动 |
|---|---|
`api.go` `TypedPlane` | +`Speech` +`Transcriptions` 两方法。**不加 `api.New` 参数**：现有 4 个调用点含 3 个测试传 `nil, nil`，加接口方法 `*proxy.Handler` 自动满足，改签名要动 4 处 |
`api.go` `/v1` 组 | `vr.Post("/audio/speech")` + `vr.Post("/audio/transcriptions")`；VK 鉴权/限流/预算三件中间件已在组上，自动生效 |
`models.go` `validModelTypes` | +`tts` +`stt` |
`models.go` `validBillingModes` | +`audio_second` +`char` |
`models.go` create/update | 读写 `audio_sec_price` `char_price`；两处报错文案同步 |
`routes.go` endpoint 白名单（create + update 两处） | +`tts` +`stt` |
`routes.go` `validateFallbackModel` | +`case "tts"` `case "stt"` |
`routes.go` `endpointToProtocol` | 落 default → `completions`，无需改 |

---

## 8. 运行层（`internal/config/runtime.go`）

```go
{key: "audio.max_upload_mb", def: `25`, validate: intRange(1, 200)},
```
默认 25（OpenAI 官方上限）；Groq dev tier 100MB 可调高。

---

## 9. 前端（`web/src/`）

| 文件 | 改动 |
|---|---|
`constants/endpoints.ts` | `ENDPOINT_META` +`tts`(语音合成) +`stt`(语音识别)；`FAMILY_TABS` +`{key:'audio',label:'语音',endpoints:['tts','stt']}` |
`ConfigCenter.tsx` | `modelTypeOptions` +2；`modelTypeTag` +2 色；价格区按 `billing_mode` 切标签（每音频秒 / 每百万字符 / 每次 / 每百万 token） |
`Routes.tsx` | 两处 endpoint→type 过滤各 +2 分支；curl 生成器加音频分支（STT 用 `-F file=@audio.wav -F model=...`；TTS 用 `--output out.mp3`），形态与 JSON 分支不同，独立函数处理 |
`Logs.tsx` / `Stats.tsx` | 零改动（族过滤由 `FAMILY_TABS` 驱动） |
`Playground.tsx` | 本期不做（需上传控件 + `<audio>` 播放器 + `response_format`→blob MIME 映射） |

---

## 10. 刻意不做（透传原则）

与各兼容端点的格式差异一律不改写，原样透传：

- OpenRouter TTS 仅支持 `mp3`/`pcm` 且默认 `pcm`（OpenAI 默认 `mp3`）→ 不注入默认值。
- OpenRouter STT 仅认 `json`/`verbose_json`，`text`/`srt`/`vtt` 返 400。
- Groq TTS 仅英文/沙特阿语，`response_format` 默认 `wav`。
- `gpt-4o-transcribe` 系仅支持 `json`。
- 上游约 60s 超时（OpenRouter 明示），长音频由客户端切分。

网关唯一改写：`model` 字段（逻辑路由名 → 物理模型名）+ 模型级 `body_override` 合并。与 `embeddings.go` 既定的"非 chat 家族不做跨厂商转换"方针一致。

---

## 11. 测试

| 测试文件 | 覆盖 |
|---|---|
`audiolen_test.go` | 5 种容器各造合法头断言秒数；截断/伪造 → `(0,false)`；表驱动 |
`audio_test.go` | TTS：上游 mp3 字节 → 客户端收到同字节 + `Content-Type` 保留；SSE 分支逐帧透传 + done usage；200-JSON 错误分支<br/>STT：上游收到 multipart、`model` 已替换为物理名、`file` part 字节与 filename 未变；`verbose_json` usage → `AudioSeconds`；本地探测兜底；超限 → 413<br/>路由：`tts`/`stt` 只挑同 type 后端；`all_backends_unavailable`；fallback 生效 |
`cost_test.go` | `audio_second` / `char` / `per_call` 三分支金额断言 |
api 侧 | 建 `tts` 模型挂 `embedding` 目标 → 400；endpoint 白名单含 `tts`/`stt` |

---

## 12. 交付顺序与验证

1. 后端（§4–§8）→ `go build ./... && go vet ./... && go test ./...`
2. 冒烟：起 dev 实例（`:27777`），建 Groq 提供商 + `whisper-large-v3-turbo`(stt) / `orpheus-v1-english`(tts) 模型 + 两条路由，curl 打 `/v1/audio/*`，查 `/api/logs` 确认 `endpoint=tts/stt`、`audio_seconds`、`input_chars`、`cost`
3. 前端（§9）→ `cd web && npm run build` → 浏览器过模型表单 / 路由表单 / 日志族过滤 / curl 示例
4. 文档：`docs/design.md`（G1 端点清单、§3.2 DDL、§7.1 代理面、§9.2 运行层）+ 本文档转「已完成」
