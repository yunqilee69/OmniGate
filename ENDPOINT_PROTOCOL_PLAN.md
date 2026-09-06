# Endpoint Protocol Refactor Plan

## Context

User requests five changes to model and route management:
1. Add custom API path override input field in model form to replace auto-constructed paths
2. Change model protocol options from `openai/responses/anthropic` to `completions/responses/messages`
3. Update "Get Models" button tooltip to indicate it appends `/v1/models` to provider's `host:port`
4. Add request body override JSON field to both routes and models for runtime request rewriting
5. Unify endpoint type naming: route `endpoint` field currently uses `chat/messages/responses`; align model selection logic so `chat` endpoint can use all model types with adaptation, while `responses/messages` endpoints only accept matching protocol models

Current system has three-layer protocol/type/endpoint model:
- **Model.Protocol** (`openai/responses/anthropic`): outbound wire format
- **Model.Type** (`chat/embedding/rerank`): functional category
- **Route.Endpoint** (`chat/messages/responses`): inbound API path family

Target: unify protocol naming to match actual API paths (`completions/responses/messages`), add path/body override fields, clarify cross-protocol routing rules.

## Approach

### 1. Add model API path override field

**Database**: Add `Model.ApiPath` string field (nullable, default empty) to store custom endpoint override.

**Backend**: 
- Migration: add column `api_path VARCHAR(512) DEFAULT ''` to `models` table
- `internal/store/models.go`: add `ApiPath string` field with `json:"api_path" gorm:"size:512;not null;default:''"`
- `internal/proxy/adapter.go`: each adapter's `endpoint()` method checks `model.ApiPath`; if non-empty use it directly, else construct default path
- Validation in `internal/api/models.go` create/update: if `ApiPath` provided, must be valid URL path (start with `/` or `http://`/`https://`)

**Frontend**:
- `web/src/pages/ConfigCenter.tsx` ModelsTab form: add `Form.Item` for `api_path` below protocol field
- Label: "API 路径覆盖（选填）"
- Extra: "留空使用协议默认路径；填写时可用完整 URL 或相对路径（如 /custom/chat）"
- Placeholder: "如 /v1/custom/chat 或 https://custom.example.com/chat"

### 2. Rename protocol values from openai/responses/anthropic to completions/responses/messages

**Database migration**: rename existing protocol values in `models` table:
- `openai` → `completions`
- `responses` → `responses` (unchanged)
- `anthropic` → `messages`

**Backend code changes** (exact replacements):
- `internal/store/models.go:36` comment: change "openai(chat/completions) | responses(OpenAI Responses) | anthropic(messages)" to "completions(/chat/completions) | responses(/responses) | messages(/messages)"
- `internal/api/models.go:21` validProtocols map: change keys to `{"completions": true, "responses": true, "messages": true}`
- `internal/proxy/adapter.go:597` AdapterFor function: change case values:
  - `case "messages":` → `return newAnthropicAdapter()`
  - `case "responses":` → `return newResponsesAdapter()`
  - `case "completions":` → (add) `return openaiAdapter{}`
  - `default:` → return error or openaiAdapter for backward compat
- All test files in `internal/proxy/*_test.go`: replace `"openai"` → `"completions"`, `"anthropic"` → `"messages"` in seed/test data
- `internal/api/routes.go:120-129` endpointToProtocol: change return values:
  - `"chat"` → `"completions"`
  - `"responses"` → `"responses"`
  - `"messages"` → `"messages"`

**Frontend**:
- `web/src/pages/ConfigCenter.tsx:57-61` protocolOptions: change to:
  ```typescript
  const protocolOptions = [
    { value: 'completions', label: 'completions — OpenAI /chat/completions' },
    { value: 'responses', label: 'responses — OpenAI /responses' },
    { value: 'messages', label: 'messages — Anthropic /messages' },
  ]
  ```
- Update line 631 protocol change handler: keep `form.setFieldValue('protocol', 'completions')` for non-chat types (was 'openai')
- Update line 645 disabled select option value from `'openai'` to `'completions'`

### 3. Update fetch models tooltip and implementation

**Frontend**: `web/src/pages/ConfigCenter.tsx:573`
- Change tooltip from current text to: "从提供商的 {host:port}/v1/models 获取可用模型列表"
- Implementation already correct at line 471: POSTs to `/api/providers/fetch-models` with `base_url` which backend appends `/v1/models` to

**Backend**: verify `internal/api/fetch_models.go:56-66` already implements correct behavior (confirmed: appends `/v1/models` or `/models` based on existing `/v1` suffix)

### 4. Add request body override JSON fields to routes and models

**Database**:
- Migration: add `body_override TEXT DEFAULT ''` to both `models` and `routes` tables
- `internal/store/models.go:39`: add `BodyOverride string` field with `json:"body_override" gorm:"type:text;not null;default:''"`
- `internal/store/models.go:80`: add same field to Route struct

**Backend logic**:
- `internal/proxy/adapter.go`: each adapter's `buildBody()` method receives the constructed request body, then merges `model.BodyOverride` (if valid JSON), then `route.BodyOverride` (if valid JSON) on top
- Merge order: base body → model override → route override (route wins on conflicts)
- Invalid JSON in override field logs warning and skips that override layer
- Empty string skips merge
- Helper function `mergeBodyOverride(base map[string]any, overrideJSON string) map[string]any` in `internal/proxy/adapter.go`:
  - Parse `overrideJSON` to map
  - Shallow merge: for each key in override, set `base[key] = override[key]`
  - Return merged base

**Validation** in `internal/api/models.go` and `internal/api/routes.go` create/update:
- If `body_override` provided and non-empty, attempt `json.Unmarshal` to `map[string]any`
- Return 400 if parse fails with message "body_override 必须是有效的 JSON 对象"

**Frontend**:
- `web/src/pages/ConfigCenter.tsx` ModelsTab form: add `Form.Item` for `body_override` after `key_ids`
  - Label: "请求体覆盖（选填，JSON 格式）"
  - Extra: "转发时合并到请求体，相同字段覆盖，新字段添加"
  - Component: `Input.TextArea` with `rows={3}` and placeholder `{"temperature": 0.7}`
  - Add validation rule to check valid JSON when non-empty
- `web/src/pages/Routes.tsx:51-86` form: add same field in route form after `remark` field
  - Same label/extra/validation as model form


### 5. Unify route endpoint selection logic

**Database migration**: rename route endpoint values in `routes` table:
- `chat` → `completions`
- `responses` → `responses` (unchanged)
- `messages` → `messages` (unchanged)

**Backend changes**:
- `internal/store/models.go:79` comment: change "chat(/v1/chat/completions) | messages(/v1/messages) | responses(/v1/responses)" to "completions(/v1/chat/completions) | messages(/v1/messages) | responses(/v1/responses)"
- `internal/store/models.go:83` field default: change from `'chat'` to `'completions'`
- `internal/api/routes.go:83-118` validateTargets: rewrite protocol matching logic:
  ```go
  requiredProtocol := endpointToProtocol(endpoint)
  for _, m := range models {
    // completions endpoint accepts all protocols (adapter converts)
    // responses/messages endpoints require exact protocol match
    if endpoint != "completions" && m.Protocol != requiredProtocol {
      return false, fmt.Sprintf("端点 %s 要求模型协议为 %s，但模型 %s 的协议是 %s", 
        endpoint, requiredProtocol, m.Name, m.Protocol)
    }
  }
  ```
- `internal/api/routes.go:120-129` endpointToProtocol: change to identity function since names now match:
  ```go
  func endpointToProtocol(endpoint string) string {
    return endpoint // completions/responses/messages map to themselves
  }
  ```
- `internal/router/router.go:209-211` Pick method: change type filter from `"chat"` to `"completions"` (Note: this is the Model.Type field filter, not endpoint)
- `internal/proxy/proxy.go:927,933,989-991` nativeEndpoint: Route.Endpoint stores short name (`messages`, `responses`, `completions`), but function parameter `endpoint` is full path (`/v1/messages`, `/v1/responses`). Comparison at line 989 needs adjustment:
  - Change Messages handler line 927: first parameter changes from `"anthropic"` to `"messages"`, keep second as `"/v1/messages"`
  - Change Responses handler line 933: first parameter stays `"responses"`, keep second as `"/v1/responses"`
  - Line 989 comparison: map full path to short name for comparison:
    ```go
    shortEndpoint := strings.TrimPrefix(strings.TrimPrefix(endpoint, "/v1/"), "/")
    if snap.Route.Endpoint != shortEndpoint {
      openAIError(w, http.StatusBadRequest, "endpoint_mismatch",
        fmt.Sprintf("route '%s' is configured for endpoint '%s', but you called '%s'", 
          routeName, snap.Route.Endpoint, shortEndpoint), nil)
      return
    }
    ```

**Frontend**:
- `web/src/pages/Routes.tsx:143,149-152` endpoint select: change initialValue from `"chat"` to `"completions"` and update options:
  ```typescript
  <Form.Item name="endpoint" label="端点类型" initialValue="completions" rules={[{ required: true }]}
    extra="决定代理路径与协议：completions 支持所有协议模型（自动适配），messages/responses 仅支持对应协议">
    <Select options={[
      { value: 'completions', label: 'completions — /v1/chat/completions（支持所有协议）' },
      { value: 'messages', label: 'messages — /v1/messages（仅 messages 协议）' },
      { value: 'responses', label: 'responses — /v1/responses（仅 responses 协议）' },
    ]} />
  </Form.Item>
  ```
- Update line 161-165 protocol filtering logic:
  ```typescript
  const endpoint = getFieldValue('endpoint') || 'completions'
  // completions accepts all protocols; messages/responses require exact match
  const requiredProtocol = endpoint === 'completions' ? null 
    : endpoint === 'messages' ? 'messages' 
    : 'responses'
  const filteredModels = requiredProtocol 
    ? models.filter((m) => m.protocol === requiredProtocol)
    : models  // completions: all models available
  ```
- Update line 200 disabled message: change to show "所有模型已选择" for completions, or "所有 X 协议模型已选择" for specific protocols

## Critical files & anchors

1. **internal/store/models.go:39-55,78-88** — Model and Route struct definitions; add ApiPath and BodyOverride string fields (empty default)
2. **internal/proxy/adapter.go:15-25,31-45,597-606** — ProtocolAdapter interface endpoint() and buildBody() methods; add path override check in each adapter's endpoint(), add body merge in buildBody() or proxy caller
3. **internal/api/routes.go:82-129** — validateTargets and endpointToProtocol; rewrite to allow completions endpoint to accept any protocol, others require exact match
4. **internal/api/models.go:25-34,182-191,94-180,193-320** — model create/update validation; add ApiPath URL validation and BodyOverride JSON validation
5. **internal/proxy/proxy.go:380-420,924-994** — proxy request building and nativeEndpoint; integrate model.ApiPath into adapter.endpoint() call, merge BodyOverride layers, fix endpoint name comparison
6. **web/src/pages/ConfigCenter.tsx:450-701** — ModelsTab form; add ApiPath input and BodyOverride textarea, update protocolOptions to new names
7. **web/src/pages/Routes.tsx:136-210** — route form; add BodyOverride textarea, update endpoint options and model filtering logic for completions-accepts-all


## Verification

### Manual UI verification:
1. Start dev server, navigate to Config Center
2. Create new provider with base_url `http://localhost:8080`
3. Click "获取模型" button, verify tooltip shows "从提供商的 {host:port}/v1/models 获取可用模型列表"
4. Create model with protocol `completions`, verify dropdown shows three options with new names
5. Fill API path override with `/custom/chat`, save, verify stored
6. Fill body override with `{"temperature": 0.9}`, save, verify validation accepts valid JSON and rejects invalid
7. Navigate to Routes page
8. Create route with endpoint `completions`, verify can select models with any protocol
9. Create route with endpoint `messages`, verify only messages-protocol models selectable
10. Fill route body override field, verify same validation behavior

### Automated verification:
Run existing test suite with protocol value updates:
```bash
cd internal/api && go test -v
cd internal/proxy && go test -v
cd internal/router && go test -v
```

All tests should pass after seed data protocol values updated to new names.

### End-to-end proxy verification:
1. Create completions route with one completions-protocol model and one messages-protocol model (cross-protocol)
2. Send request to `/v1/chat/completions` with that route
3. Verify adapter correctly converts request based on selected model's protocol
4. Check logs confirm body_override fields merged into outbound request

## Assumptions & contingencies

- **Protocol rename migration timing**: assume no concurrent writes during migration; if deployment requires zero-downtime, add temporary dual-protocol support (accept both old and new values) for one release cycle, then remove old values
- **API path override validation**: assumes any string starting with `/` or `http` is valid; if stricter validation needed (e.g., must parse as valid URL), add `url.Parse()` check in validation
- **Body override merge depth**: shallow merge only (top-level keys); if nested merge required, implement recursive merge function
- **Adapter buildBody signature change**: if adding model/route context to buildBody breaks existing adapter interface, add new method `buildBodyWithOverrides()` and call from proxy layer instead of modifying existing method
