// Package store 定义全部实体模型与 SQLite 存取。
// 时间戳一律使用 unix 秒（int64 + GORM autoCreateTime/autoUpdateTime）。
package store

// Provider 大模型提供商。
type Provider struct {
	ID        int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	Name      string `json:"name" gorm:"size:191;not null;uniqueIndex"`
	BaseURL   string `json:"base_url" gorm:"column:base_url;size:512;not null"`
	Protocol  string `json:"protocol" gorm:"size:32;not null;default:openai"`
	ProxyURL  string `json:"proxy_url" gorm:"column:proxy_url;size:512;not null;default:''"`
	TimeoutMs int    `json:"timeout_ms" gorm:"not null;default:120000"`
	Remark    string `json:"remark" gorm:"size:1024;not null;default:''"`
	CreatedAt int64  `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt int64  `json:"updated_at" gorm:"autoUpdateTime"`
}

// ApiKey 密钥：直接归属某提供商，由模型通过 ModelKey 多对多绑定。KeyValue 带 json:"-"，
// 任何意外的 json 序列化都不会泄露密钥值。
type ApiKey struct {
	ID             int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	ProviderID     int64  `json:"provider_id" gorm:"not null;index"`
	KeyValue       string `json:"-" gorm:"column:key_value;size:512;not null"`
	Name           string `json:"name" gorm:"size:191;not null;default:''"`
	Status         string `json:"status" gorm:"size:32;not null;default:active"` // active | cooldown | disabled
	CooldownUntil  int64  `json:"cooldown_until" gorm:"not null;default:0"`
	RateLimitCount int    `json:"rate_limited_count" gorm:"column:rate_limited_count;not null;default:0"`
	DisableReason  string `json:"disable_reason" gorm:"size:512;not null;default:''"`
	LastUsedAt     int64  `json:"last_used_at" gorm:"not null;default:0"`
	LastError      string `json:"last_error" gorm:"size:512;not null;default:''"`
	CreatedAt      int64  `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt      int64  `json:"updated_at" gorm:"autoUpdateTime"`
}

// Model 真实模型。阶梯熔断状态机挂在这一层（跨路由共享的物理事实）。
// Protocol 决定上游调用格式：openai(chat/completions) | responses(OpenAI Responses) | anthropic(messages)。
// Type 决定端点家族：chat(/v1/chat/completions) | embedding(/v1/embeddings) | rerank(/v1/rerank)；
// embedding/rerank 仅支持 protocol=openai（业界无可归一标准，按各自事实骨架直通）。
type Model struct {
	ID            int64   `json:"id" gorm:"primaryKey;autoIncrement"`
	ProviderID    int64   `json:"provider_id" gorm:"not null;uniqueIndex:idx_model_provider_name"`
	Name          string  `json:"name" gorm:"size:191;not null;uniqueIndex:idx_model_provider_name"`
	Type          string  `json:"type" gorm:"size:32;not null;default:'chat'"` // chat | embedding | rerank
	Protocol      string  `json:"protocol" gorm:"size:32;not null;default:openai"`
	InputPrice    float64 `json:"input_price" gorm:"not null;default:0"`               // 每 1M prompt token 价格
	OutputPrice   float64 `json:"output_price" gorm:"not null;default:0"`              // 每 1M completion token 价格
	PriceCurrency string  `json:"price_currency" gorm:"size:8;not null;default:'USD'"` // 价格币种：USD | CNY；计费时统一折算为 USD 入库
	Status        string  `json:"status" gorm:"size:32;not null;default:active"`       // active | cooldown | disabled
	FailCount     int     `json:"fail_count" gorm:"not null;default:0"`
	CooldownUntil int64   `json:"cooldown_until" gorm:"not null;default:0"`
	DisableReason string  `json:"disable_reason" gorm:"size:512;not null;default:''"`
	LastError     string  `json:"last_error" gorm:"size:512;not null;default:''"`
	CreatedAt     int64   `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt     int64   `json:"updated_at" gorm:"autoUpdateTime"`
}

// ModelKey 模型 × 密钥 多对多关联。
type ModelKey struct {
	ModelID int64 `json:"model_id" gorm:"primaryKey"`
	KeyID   int64 `json:"key_id" gorm:"primaryKey"`
}

// Route 逻辑路由（客户端请求的 modelId）。
// Endpoint 决定协议族：chat(/v1/chat/completions) | messages(/v1/messages) | responses(/v1/responses)。
type Route struct {
	ID        int64         `json:"id" gorm:"primaryKey;autoIncrement"`
	Name      string        `json:"name" gorm:"size:191;not null;uniqueIndex"`
	Endpoint  string        `json:"endpoint" gorm:"size:32;not null;default:chat"` // chat | messages | responses
	Remark    string        `json:"remark" gorm:"size:1024;not null;default:''"`
	Targets   []RouteTarget `json:"targets" gorm:"foreignKey:RouteID"`
	CreatedAt int64         `json:"created_at" gorm:"autoCreateTime"`
	UpdatedAt int64         `json:"updated_at" gorm:"autoUpdateTime"`
}

// RouteTarget 路由目标：路由 → 真实模型（带权重）。
type RouteTarget struct {
	ID      int64 `json:"id" gorm:"primaryKey;autoIncrement"`
	RouteID int64 `json:"route_id" gorm:"not null;index:idx_rt_route;uniqueIndex:idx_rt_route_model"`
	ModelID int64 `json:"model_id" gorm:"not null;uniqueIndex:idx_rt_route_model"`
	Weight  int   `json:"weight" gorm:"not null;default:1"`
}

// AppConfig 运行层配置（key-value，value 为 JSON 编码）。
type AppConfig struct {
	Key   string `json:"key" gorm:"primaryKey;column:key;size:64"`
	Value string `json:"value" gorm:"not null"`
}

func (AppConfig) TableName() string { return "app_config" }

// RequestLog 请求日志（统计事实表，只增不改；表结构上不存在任何请求内容字段）。
type RequestLog struct {
	ID               int64   `json:"id" gorm:"primaryKey;autoIncrement"`
	CreatedAt        int64   `json:"created_at" gorm:"autoCreateTime;index:idx_rl_time_route,priority:1;index:idx_rl_time_provider,priority:1"`
	Status           string  `json:"status" gorm:"size:32;not null"`
	Route            string  `json:"route" gorm:"size:191;not null;index:idx_rl_route;index:idx_rl_time_route,priority:2"`
	Provider         string  `json:"provider" gorm:"size:191;not null;index:idx_rl_provider;index:idx_rl_time_provider,priority:2"`
	Model            string  `json:"model" gorm:"size:191;not null"`
	KeyID            int64   `json:"key_id" gorm:"not null;default:0;index:idx_rl_key"`
	RequestID        string  `json:"request_id" gorm:"size:64;not null"`
	ErrorCode        string  `json:"error_code" gorm:"size:64;not null;default:''"`
	IsStream         bool    `json:"is_stream" gorm:"not null;default:false"`
	IsFallback       bool    `json:"is_fallback" gorm:"not null;default:false"`
	TokensEstimated  bool    `json:"tokens_estimated" gorm:"not null;default:false"`
	Retries          int     `json:"retries" gorm:"not null;default:0"`
	PromptTokens     int     `json:"prompt_tokens" gorm:"not null;default:0"`
	CompletionTokens int     `json:"completion_tokens" gorm:"not null;default:0"`
	TTFTMs           int64   `json:"ttft_ms" gorm:"not null;default:0"`
	TotalMs          int64   `json:"total_ms" gorm:"not null;default:0"`
	Cost             float64 `json:"cost" gorm:"not null;default:0"`
	ErrorBody        string  `json:"error_body,omitempty" gorm:"type:text"`
}

// ContentLog 内容日志（可选；全局与路由白名单开关均开启时才写入）。
type ContentLog struct {
	RequestID    string `json:"request_id" gorm:"primaryKey;column:request_id;size:64"`
	Route        string `json:"route" gorm:"size:191;not null"`
	RequestBody  string `json:"request_body" gorm:"type:text;not null"`
	ResponseBody string `json:"response_body" gorm:"type:text;not null"`
	CreatedAt    int64  `json:"created_at" gorm:"autoCreateTime;index"` // 保留期清理按时间扫描
}

// RequestAttempt 单次尝试的明细记录（含中间失败与最终成功）。request_log 仍记最终结果与总重试次数，
// attempt 表逐次落盘：失败重试链路可在日志详情里完整回放。
type RequestAttempt struct {
	ID               int64  `json:"id" gorm:"primaryKey;autoIncrement"`
	RequestID        string `json:"request_id" gorm:"size:64;not null;index:idx_ra_request,priority:1"`
	Attempt          int    `json:"attempt" gorm:"not null;index:idx_ra_request,priority:2"`
	Route            string `json:"route" gorm:"size:191;not null"`
	Model            string `json:"model" gorm:"size:191;not null"`
	Provider         string `json:"provider" gorm:"size:191;not null"`
	KeyID            int64  `json:"key_id" gorm:"not null;default:0"`
	Status           string `json:"status" gorm:"size:32;not null"`
	HTTPStatus       int    `json:"http_status" gorm:"not null;default:0"`
	ErrorCode        string `json:"error_code" gorm:"size:64;not null;default:''"`
	ErrorBody        string `json:"error_body,omitempty" gorm:"type:text"`
	LatencyMs        int64  `json:"latency_ms" gorm:"not null;default:0"`
	TTFTMs           int64  `json:"ttft_ms" gorm:"not null;default:0"`
	PromptTokens     int    `json:"prompt_tokens" gorm:"not null;default:0"`
	CompletionTokens int    `json:"completion_tokens" gorm:"not null;default:0"`
	CreatedAt        int64  `json:"created_at" gorm:"autoCreateTime;index"`
}

// RequestLogDaily 请求日志按“天 × 全维度 × 状态”的预聚合表（每行一条 UPSERT）。
// 统计接口优先读这张表：overview/breakdown 从 O(请求数) 降到 O(天×维度组合)。
// p95 用 TTFTB0..9 / TotalB0..9 直方桶反查，避免把全部 ttft_ms / total_ms 拉进 Go 排序。
type RequestLogDaily struct {
	Day              int64   `gorm:"primaryKey;column:day"`
	Route            string  `gorm:"primaryKey;size:191;default:''"`
	Model            string  `gorm:"primaryKey;size:191;default:''"`
	Provider         string  `gorm:"primaryKey;size:191;default:''"`
	Status           string  `gorm:"primaryKey;size:32;default:''"`
	Total            int64   `gorm:"not null;default:0"`
	Success          int64   `gorm:"not null;default:0"`
	Errors           int64   `gorm:"not null;default:0"`
	PromptTokens     int64   `gorm:"not null;default:0"`
	CompletionTokens int64   `gorm:"not null;default:0"`
	Cost             float64 `gorm:"not null;default:0"`
	RetriesSum       int64   `gorm:"not null;default:0"`
	TTFTB0           int64   `gorm:"not null;default:0;column:ttftb0"`
	TTFTB1           int64   `gorm:"not null;default:0;column:ttftb1"`
	TTFTB2           int64   `gorm:"not null;default:0;column:ttftb2"`
	TTFTB3           int64   `gorm:"not null;default:0;column:ttftb3"`
	TTFTB4           int64   `gorm:"not null;default:0;column:ttftb4"`
	TTFTB5           int64   `gorm:"not null;default:0;column:ttftb5"`
	TTFTB6           int64   `gorm:"not null;default:0;column:ttftb6"`
	TTFTB7           int64   `gorm:"not null;default:0;column:ttftb7"`
	TTFTB8           int64   `gorm:"not null;default:0;column:ttftb8"`
	TTFTB9           int64   `gorm:"not null;default:0;column:ttftb9"`
	TotalB0          int64   `gorm:"not null;default:0;column:totalb0"`
	TotalB1          int64   `gorm:"not null;default:0;column:totalb1"`
	TotalB2          int64   `gorm:"not null;default:0;column:totalb2"`
	TotalB3          int64   `gorm:"not null;default:0;column:totalb3"`
	TotalB4          int64   `gorm:"not null;default:0;column:totalb4"`
	TotalB5          int64   `gorm:"not null;default:0;column:totalb5"`
	TotalB6          int64   `gorm:"not null;default:0;column:totalb6"`
	TotalB7          int64   `gorm:"not null;default:0;column:totalb7"`
	TotalB8          int64   `gorm:"not null;default:0;column:totalb8"`
	TotalB9          int64   `gorm:"not null;default:0;column:totalb9"`
	UpdatedAt        int64   `gorm:"not null;default:0"`
}

func (RequestLogDaily) TableName() string { return "request_log_daily" }
