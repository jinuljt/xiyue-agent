# 喜悦 Agent

喜悦 Voice 的音箱端运行程序。它提供 HTTPS 控制台、语音对话、倒计时、闹钟、待办、自动化、远程听音和第三方调用计量。语音服务可由用户配置，音箱也可把复杂任务交给外部 Agent。

## 本地运行

需要 Go 1.25 或更新版本。把 [xiyue-console](https://github.com/jinuljt/xiyue-console) 克隆到本仓库旁边。

```sh
go run ./cmd/xiyue-agent -console ../xiyue-console/web
```

访问 [本地控制台](http://127.0.0.1:8765/)。默认启动本机开发服务，数据保存到 `data/state.json`，音箱录音、唤醒与系统播报需要设备适配。正式设备使用 HTTPS 和管理员登录，安装预检见 [xiyue-flasher](https://github.com/jinuljt/xiyue-flasher)。

## 设备能力接口

在设备控制台的 **系统配置 → 外部访问** 创建授权。外部系统可通过 HTTPS API 或 MCP 发现并调用已授权的能力。接口支持查询设备、远程听音、倒计时、闹钟、待办与自动化状态，也可按授权创建倒计时、运行指定自动化。

查看 [接口、权限和示例](docs/capability-api.md)。

## 仓库目录

| 路径 | 内容 |
| --- | --- |
| [`cmd/xiyue-agent/`](cmd/xiyue-agent/) | 音箱运行程序、控制台后端、语音适配、调度和测试 |
| [`docs/`](docs/) | 外部设备接口契约 |
| [`go.mod`](go.mod) | Go 依赖 |

```sh
go test ./...
go vet ./...
go build ./cmd/xiyue-agent
```

本地状态、对话和服务凭据属于运行数据，应保存在仓库之外。
