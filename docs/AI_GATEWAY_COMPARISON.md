# ULW-Research Synthesis: AI gateway capability comparison

Workers: orchestrator · Waves: 3 · Sources: 14 observations · Verifications: source/counter-search

## Executive summary

OmniGate is not broadly behind on gateway mechanics. Its weighted logical-model routing, model/key health state, bounded pre-first-byte failover, protocol-aware forwarding, privacy-default request logging, and per-attempt diagnostics are coherent strengths ([OmniGate router](https://github.com/yunqilee69/OmniGate/blob/5233305a48fab1df9b69e2517c8bc930b1089842/internal/router/router.go#L189-L237), [proxy](https://github.com/yunqilee69/OmniGate/blob/5233305a48fab1df9b69e2517c8bc930b1089842/internal/proxy/proxy.go#L265-L331)).

The largest practical gap is the missing policy plane. OmniGate explicitly excludes users, quota billing, and per-user/key limits in v1 ([design](https://github.com/yunqilee69/OmniGate/blob/5233305a48fab1df9b69e2517c8bc930b1089842/docs/design.md#L27-L33)). The comparison projects converge on virtual consumer credentials, hierarchical budgets, request/token limits, model/provider access policy, cache, guardrails, and exportable telemetry. The recommended sequence is: consumer identity and enforceable quotas first; standard telemetry and cache second; guardrails/policy routing third; clustering and richer protocols only after those foundations.

## Capability matrix

| Capability | OmniGate | one-api / new-api | LiteLLM | Bifrost | OpenRouter | Portkey | Kong AI Gateway |
|---|---|---|---|---|---|---|---|
| Weighted routing / failover | Strong, local | Channel/group routing | Many strategies + fallbacks | Load balancing + fallback | Price/health/provider policy | Config strategies + fallback | Plugin balancers + fallback |
| Consumer virtual keys | No | Yes | Yes | Yes | API keys/workspaces | API keys/configs | Consumers/access tiers |
| Spend budgets / quotas | Reporting only | Token quota | User/team/key budgets | Hierarchical budgets | Account/key credits | Budget limits | Cost-based limits/billing |
| Request/token rate limits | No per-user/key limit | Platform/group features | RPM/TPM/model limits | VK/provider request+token limits | Platform/provider limits | Hour/day/min request+token limits | Consumer/model/provider policies |
| Exact / semantic cache | No response cache | Not a central strength | Multiple backends + semantic | Exact + semantic | Not a primary gateway feature | Simple + semantic | Exact/semantic plugin |
| Guardrails / data policy | No hook | Limited policy surface | Pluggable guardrails | Enterprise governance | ZDR/data-collection routing | Input/output guardrails | PII, prompt/response guards |
| Observability export | SQLite/UI, privacy default | Usage/admin metrics | Logging/callback ecosystem | Prometheus/OTel/logging | Broadcast integrations | OTel-compliant logs | OTLP/metrics/audit/Konnect |
| Tenancy / SSO / RBAC | Admin auth only | Users/groups/OIDC in new-api | Users/teams/orgs/SSO | Teams/customers/OIDC (edition scoped) | Workspaces/members | Hosted control plane | Consumers/groups/workspaces |
| Protocol breadth | Chat, embeddings, rerank, Anthropic/Responses | Broad provider adapters | Very broad provider/pass-through | Broad provider/native/async APIs | Broad managed catalog | Multimodal/MCP/other APIs | LLM + MCP + A2A |

## Findings by theme

### 1. Routing and resilience

OmniGate already covers weighted target selection, key round-robin, cooldown filtering, affinity, and bounded retries. LiteLLM adds named routing strategies including least-busy, usage-, latency-, and cost-based routing ([source](https://github.com/BerriAI/litellm/blob/4ba8517134816b98a040300032e7e0185ccffbc7/litellm/router.py#L593-L677)). OpenRouter adds request-level provider constraints, price ceilings, ZDR/data policy filters, and percentile performance preferences ([docs](https://openrouter.ai/docs/guides/routing/provider-selection)).

**Gap:** not basic load balancing; it is policy-aware selection. Add hard constraints and a pluggable scoring interface before adding more algorithms.

### 2. Identity, quotas, and monetization

one-api tokens carry expiry, remaining/used quota, model allowlists, and subnet policy ([source](https://github.com/songquanpeng/one-api/blob/8df4a2670b98266bd287c698243fff327d9748cf/model/token.go#L23-L37)); new-api adds model limits, IP allowlists, groups, and cross-group retry ([source](https://github.com/QuantumNous/new-api/blob/27ff6a8767e728f879d52770c273d4f73214a430/model/token.go#L14-L32)). LiteLLM exposes user/key budgets, reset durations, RPM/TPM, model-specific budgets, parallel limits, and budget fallbacks ([source](https://github.com/BerriAI/litellm/blob/4ba8517134816b98a040300032e7e0185ccffbc7/litellm/proxy/management_endpoints/internal_user_endpoints.py#L442-L483)). Bifrost documents cumulative provider→virtual-key→team→customer enforcement and separate request/token limits ([docs](https://docs.getbifrost.ai/features/governance/budget-and-limits)).

**Gap:** this is OmniGate's clearest missing capability. Build `Consumer → Credential → Policy` with atomic pre-consumption, post-settlement, reset windows, and fail-closed behavior.

### 3. Caching

LiteLLM supports local, Redis, semantic Redis/Valkey, Qdrant, S3, disk, and dual caches ([source](https://github.com/BerriAI/litellm/blob/4ba8517134816b98a040300032e7e0185ccffbc7/litellm/caching/caching.py#L107-L128)). Bifrost separates exact hash replay from semantic similarity, supports streaming replay and TTL, and requires an explicit cache key for isolation ([docs](https://docs.getbifrost.ai/features/semantic-caching)).

**Gap:** OmniGate has session affinity intended to improve upstream cache hits, but no response cache. Start with opt-in exact caching keyed by route/model/provider/request parameters; add semantic caching only with an explicit external-store boundary and tenant isolation.

### 4. Guardrails and data policy

Portkey supports input/output checks, synchronous deny or transform actions, feedback, and fallback/retry orchestration ([docs](https://docs.portkey.ai/docs/product/guardrails)). Kong documents prompt/response guards, PII sanitization, allow/deny policies, and third-party guardrail integrations ([docs](https://developer.konghq.com/ai-gateway/)). LiteLLM's passthrough helper shows an opt-in, field-targeted, inherited guardrail model ([source](https://github.com/BerriAI/litellm/blob/4ba8517134816b98a040300032e7e0185ccffbc7/litellm/proxy/pass_through_endpoints/passthrough_guardrails.py#L28-L84)). OpenRouter provides per-request data-collection and ZDR provider constraints ([docs](https://openrouter.ai/docs/guides/routing/provider-selection)).

**Gap:** no request/response policy hook exists in OmniGate. Define a small synchronous/async middleware contract first; do not hard-code a vendor safety service.

### 5. Observability

OmniGate's local model is unusually privacy-conscious: metadata is always recorded and content capture is separately opt-in ([source](https://github.com/yunqilee69/OmniGate/blob/5233305a48fab1df9b69e2517c8bc930b1089842/docs/design.md#L437-L463)). Bifrost provides asynchronous logs, cost/token/latency fields, filtering, WebSocket updates, Prometheus, and OTel connectors ([docs](https://docs.getbifrost.ai/features/observability/default)). Kong exposes audit logs, LLM metrics, OTLP spans/metrics, and Konnect analytics ([docs](https://developer.konghq.com/ai-gateway/)).

**Gap:** export, not local measurement. Add Prometheus metrics and OTLP spans with body redaction and sampling controls; preserve SQLite as the local audit store.

### 6. Protocol and platform surface

Bifrost advertises a unified API across many providers plus MCP, multimodality, async jobs, and integrations ([README](https://github.com/maximhq/bifrost/blob/7e26cffbd47cd295f35b64176bfbb721fdd0924a/README.md#L78-L107)). Portkey documents multimodality, MCP, gRPC beta, and gateway access to other APIs ([docs](https://docs.portkey.ai/docs/product/ai-gateway)). Kong extends beyond LLMs to MCP and A2A ([docs](https://developer.konghq.com/ai-gateway/)).

**Gap:** OmniGate's roadmap already calls out MCP and native protocol expansion. These are valuable, but should follow governance hooks so new transports inherit identity, quotas, logging, and policy.

## Recommended roadmap

1. **P0 — Consumer policy plane:** virtual keys, expiry/revocation, model/provider allowlists, request RPM, token TPM, spend budgets, reset windows, atomic reservation/settlement, 429/402 response headers, and audit attribution.
2. **P1 — Exportable observability:** Prometheus endpoint, OTLP traces/metrics, correlation IDs, configurable sampling, redaction, and an explicit privacy contract for every exporter.
3. **P1 — Exact response cache:** opt-in route policy, cache key composition, TTL, invalidation, stream replay, and tenant/consumer isolation.
4. **P2 — Pluggable guardrails:** before-request and after-response hooks, deny/transform/log actions, bounded latency, streaming semantics, and policy result attribution.
5. **P2 — Policy-aware routing:** provider capability metadata, hard constraints (model parameters, region/data retention), dynamic price/latency/throughput scoring, and fallback reasons.
6. **P3 — Shared state and protocol growth:** Redis/Postgres-backed coordination, OIDC/RBAC, MCP, Gemini/native protocol adapters, batch/async APIs, and multi-instance operation.

## Edition and scope cautions

Bifrost's documentation labels hierarchical governance and multi-node synchronization as Enterprise; its OSS mode keeps critical state in memory ([budget docs](https://docs.getbifrost.ai/features/governance/budget-and-limits)). Kong's AI rate limiting is an Enterprise plugin and its policy-based mode is version-scoped ([rate-limit docs](https://developer.konghq.com/plugins/ai-rate-limiting-advanced/)). OpenRouter workspace budgets/observability are organization/workspace product features, not self-hostable source. Portkey exposes an open-source gateway, but the hosted guardrail/control-plane experience is not equivalent to the local gateway ([gateway docs](https://docs.portkey.ai/docs/product/ai-gateway)).

## Sources

1. OmniGate design and router/proxy source, SHA `5233305a48fab1df9b69e2517c8bc930b1089842`.
2. one-api token model, SHA `8df4a2670b98266bd287c698243fff327d9748cf`.
3. new-api token model, SHA `27ff6a8767e728f879d52770c273d4f73214a430`.
4. LiteLLM router, user endpoint, cache, and passthrough guardrail source, SHA `4ba8517134816b98a040300032e7e0185ccffbc7`.
5. Bifrost README/source, SHA `7e26cffbd47cd295f35b64176bfbb721fdd0924a`.
6. Bifrost governance, semantic caching, and observability docs.
7. OpenRouter provider selection and limits docs.
8. Portkey AI Gateway and Guardrails docs.
9. Kong AI Gateway and AI Rate Limiting Advanced docs.

## Epistemic instrumentation

Intent closure is recorded in `intent-diff.md`; claims C1-C7 are represented in `claim-graph.md`; observations O1-O14 are in `observation-manifest.md`; negative and edition counter-searches are recorded in `wave-3-counter-search.md`. No runtime verification was needed because final claims are source-shaped capability comparisons, not performance guarantees.
