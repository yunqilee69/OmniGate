# Debug Mode

## Overview
Debug mode provides comprehensive logging of all request/response data flowing through OmniGate, helping diagnose issues with upstream providers and request routing.

## Enabling Debug Mode

### Via Database
```sql
-- Enable debug logging
UPDATE app_config SET value='true' WHERE key='debug.stream_log';

-- Disable debug logging
UPDATE app_config SET value='false' WHERE key='debug.stream_log';
```

After updating the config, restart the service:
```bash
omnigate restart
```

### Via SQLite CLI
```bash
sqlite3 ~/.omnigate/omnigate.db "UPDATE app_config SET value='true' WHERE key='debug.stream_log'"
```

## What Gets Logged

When debug mode is enabled, OmniGate logs detailed information at each stage of request processing:

### 1. Outbound Request
```
[DEBUG] Outbound request to upstream
  provider: LLM Proxy
  model: glm-5
  key_id: 1
  protocol: openai
  endpoint: http://127.0.0.1:33391/v1/chat/completions
  timeout_ms: 120000
  is_stream: true
  request_body: {"messages":[...], "model":"glm-5", ...}
```

### 2. Request Headers (Sanitized)
```
[DEBUG] Request headers
  headers: {
    "Content-Type": "application/json",
    "Authorization": "Bearer sk-...",  // Truncated for security
    "Accept": "text/event-stream"
  }
```

### 3. Response Status
```
[DEBUG] Response received
  status_code: 200
  status: 200 OK
  headers: {
    "Content-Type": "text/event-stream",
    "X-Request-Id": "...",
    ...
  }
```

### 4. Stream Chunks (for streaming responses)
```
[DEBUG] Stream chunk received
  model: glm-5
  provider: LLM Proxy
  chunk_size: 597
  chunk_data: data: {"id":"msg_...", "choices":[...]}
```

### 5. Stream Completion
```
[DEBUG] Stream read error
  model: glm-5
  provider: LLM Proxy
  error: EOF
  is_eof: true
  committed: true
```

### 6. Buffered Response (for non-streaming)
```
[DEBUG] Buffered response completed
  model: glm-5
  provider: LLM Proxy
  status_code: 200
  body_size: 1234
  response_preview: {"id":"chatcmpl-...", ...}  // First 500 chars
```

### 7. Request Failures
```
[DEBUG] Request failed
  error: connection refused
  timed_out: false
  model: glm-5
```

## Viewing Logs

Debug logs are written to the standard log file:

```bash
# Follow logs in real-time
tail -f ~/.omnigate/omnigate.log

# Filter for debug entries only
tail -f ~/.omnigate/omnigate.log | grep DEBUG

# View recent debug logs
tail -100 ~/.omnigate/omnigate.log | grep DEBUG
```

## Security Notes

- **API Keys**: Authorization headers are automatically truncated to first 10 characters
- **Sensitive Headers**: Any header containing "auth" or "key" is sanitized
- **Request Bodies**: Full request bodies are logged; avoid enabling debug mode with sensitive user data in production

## Use Cases

### Diagnosing Upstream Provider Issues
Check if requests are reaching the provider correctly and what responses are received.

### Understanding Protocol Conversion
See how requests are transformed between different protocols (OpenAI, Anthropic, etc.).

### Debugging Streaming Issues
Track each SSE chunk to identify where streaming breaks or data is corrupted.

### Investigating "Successful but Logged as Error" Issues
The original issue that prompted this feature: successful responses appearing as errors in logs. Debug mode reveals:
- Actual HTTP status codes
- Complete response headers
- Stream chunk contents
- Error detection logic behavior

## Performance Impact

Debug logging adds minimal overhead:
- String formatting for log messages
- Extra I/O for writing detailed logs

For production systems with high traffic, consider:
- Enabling debug mode only temporarily when investigating issues
- Using log filtering to reduce storage requirements
- Rotating logs more frequently

## Implementation Details

Debug logging is implemented in `internal/proxy/proxy.go`:
- `attempt()` method logs outbound requests and responses
- `streamResponse()` logs each SSE chunk
- `bufferedResponse()` logs complete response bodies
- Sensitive data is automatically sanitized before logging
