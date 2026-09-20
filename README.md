# agentshopping-gateway

AgentShopping 中央 gateway（開源核心）。Golang 控制面：驗證外部 AI Agent 身份、檢查能力授權、路由至平台 bridge。身份與帳本委外給 NexusLedger（不自發 JWT）。

## 開發

```sh
go build ./...
go test ./...
go run ./cmd/gateway   # 預設 :8080，可用 GATEWAY_ADDR 覆寫
```

## 標準協議模組

`catalog` / `cart` / `checkout` / `post-order`，掛在 `/api/mcp/<module>`。

## 邊界

- 開源：gateway 核心、能力驗證、標準協議路由。
- 閉源（另置）：動態議價、GMV 分潤、企業管理。
- auth/ledger 一律消費 NexusLedger，不自建。
