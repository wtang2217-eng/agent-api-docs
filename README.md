# 代理对接资料 · 兑换接口 v1

让你自己的充值站直接完成兑换：买家在你的系统里粘贴卡密和账号信息，你的服务器调用兑换接口，买家不用再跳到我们的网站。

- **在线文档**：https://wtang2217-eng.github.io/agent-api-docs/
- **PDF**：[代理对接文档.pdf](代理对接文档.pdf)
- **接口描述文件**：[openapi.json](openapi.json)（OpenAPI 3.0，可以直接导入 Apifox、Postman）
- **示例代码**（只用语言自带的库）：[Python](examples/redeem.py) · [Node.js](examples/redeem.mjs) · [Go](examples/redeem.go)

## 接口地址

`https://recharge.chongplus.store/api/open/v1`

API 密钥（`cpa_` 开头、47 个字符）和来源 IP 登记找我们要。密钥只放在你的服务器上，**不要提交到任何代码仓库**。

每个请求都带上你自己系统的 `User-Agent`（比如 `my-shop/1.0`）：有些语言自带的默认值（Python 的 urllib、Java 8 等）会被网关直接拒绝，
回包是 HTTP 403 加一行纯文本 `error code: 1010`。详见在线文档「通用约定」。

## 四步完成一次兑换

1. `POST /cdk/query`：查卡密（可选）。
2. `POST /redeem/verify`：核对买家账号，拿到 `ticket`（约 10 秒）。
3. `POST /redeem`：用 `ticket` 加你生成的 `idempotency_key` 确认兑换，拿到 `order_no`。
4. 按 `poll_after` 秒轮询 `GET /orders/{order_no}`，直到 `succeeded` 或 `failed`。

网络超时、没拿到响应时，用**同一个** `idempotency_key` 原样再发一次，绝不会重复兑换。完整的重试规则、状态处理和错误码见在线文档。

## 运行示例

```bash
export AGENT_API_KEY=cpa_...
python examples/redeem.py <卡密> <买家 Session 文件>
node examples/redeem.mjs <卡密> <买家 Session 文件>
go run examples/redeem.go <卡密> <买家 Session 文件>
```

## 版本

v1 · 2026-10-05 首版。接口的兼容性变动我们会提前通知。
