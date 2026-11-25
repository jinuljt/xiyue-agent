# 设备能力接口 v1

设备 HTTPS 管理服务提供外部 HTTP API 和 MCP。先在控制台打开 **系统配置 → 外部访问**，为每个客户端创建独立授权并勾选所需操作。首次创建时会显示一次令牌，有效期 30 天，可随时撤销。客户端需信任设备证书，并发送 `Authorization: Bearer <令牌>`。浏览器登录状态与外部授权各自独立。

## HTTP API

| 请求 | 结果 |
| --- | --- |
| `GET /api/v1/capabilities` | 返回已授权的操作、输入和输出 JSON Schema、是否写入 |
| `POST /api/v1/capabilities/{operation}/invoke` | 执行操作，请求体为 `{"arguments":{}}`，以实际 Schema 为准 |
| `GET /api/v1/operations/{id}` | 查询原授权发起的操作，需另外勾选 `operation.get` |

例如，向 `/api/v1/capabilities/timer.create/invoke` 发送：

```json
{
  "arguments": {
    "seconds": 120,
    "name": "拉伸",
    "idempotency_key": "stretch-session-42"
  }
}
```

查询返回 200，受理后台操作返回 202 和操作 ID。外部写入必须带应用自行生成的 `idempotency_key`。重试时使用原键和原参数，同键不同参数返回 409。回执保留最多 24 小时，同时受记录条数限制。无有效授权返回 401，授权范围不足返回 403。

## MCP

MCP Streamable HTTP 入口为 `https://<音箱地址>:<HTTPS端口>/mcp`。使用支持自定义 HTTPS 请求头的客户端配置 Bearer 令牌。工具通过 `tools/list` 发现并通过 `tools/call` 调用。当前使用控制台签发令牌，客户端需手动配置。

| HTTP 操作 | MCP 工具 |
| --- | --- |
| `device.status` | `xiyue_device_status` |
| `audio.monitor.status` | `xiyue_audio_monitor_status` |
| `timer.list` | `xiyue_timer_list` |
| `alarm.list` | `xiyue_alarm_list` |
| `todo.list` | `xiyue_todo_list` |
| `automation.list` | `xiyue_automation_list` |
| `timer.create` | `xiyue_timer_create` |
| `automation.run` | `xiyue_automation_run` |
| `operation.get` | `xiyue_operation_get` |

自动化启动还需要勾选具体流程。可授权的流程仅使用受支持的设备本机动作，例如等待、播报和创建倒计时。流程动作变更后需要重新授权。设备状态读取与音箱本机语音工具使用同一份实现。
