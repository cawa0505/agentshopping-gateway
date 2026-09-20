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

`catalog` 支援 `action: "negotiate"`（階梯議價，`on_price_negotiate` hook）：gateway 原樣透傳付費 bridge 的 `/catalog/negotiate` 回應（bridge 是計價唯一權威，含每品項 `unit_price` + `price_source` tier 明細）。免費版商店無此端點，回 502 + error 欄位 — agent 應辨識並退回固定價流程。

## 採購授權模組（閉源）

`purchasing` 掛在 `/api/mcp/purchasing`，ability `purchasing:pay`，三個 action：

- `purchase` — quote-first：金額以店端 canonical 報價核對（Agent 報價不可信），原子 reserve → bridge 下單 → order-status capture。需 `binding_id` + `idempotency_key`（同 key 重放回原結果，不重複下單）。
- `get_allowance` — 額度快照（granted − captured − active reservations，由 ledger 重建）。
- `get_purchase` — 查詢採購狀態（僅限 binding 所屬 agent）。

核心在 `internal/purchasing/`（領域模型 + policy evaluator）與 `internal/store/`（SQLite：原子 reservation、append-only ledger、冪等），閉源。EXECUTING 訂單由 `ReconcileExecuting` 掃描結案（paid → CAPTURED、failed → RELEASED）。

## 邊界

- 開源：gateway 核心、能力驗證、標準協議路由。
- 閉源：採購授權核心（`internal/purchasing/`、`internal/store/`）、動態議價、GMV 分潤、企業管理。
- auth/ledger 一律消費 NexusLedger，不自建；DB 不存 raw payment credential（僅 opaque `payment_method_ref`）。
