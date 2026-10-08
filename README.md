# 代理对接资料 · 兑换接口 v1

让你自己的充值站直接完成兑换：买家在你的系统里粘贴卡密和账号信息，你的服务器调用兑换接口，买家不用再跳到我们的网站。

- 在线文档：https://wtang2217-eng.github.io/agent-api-docs/
- PDF：[代理对接文档.pdf](代理对接文档.pdf)
- 接口描述文件：[openapi.json](openapi.json)（OpenAPI 3.0，可以直接导入 Apifox、Postman）
- 示例代码：[Python](examples/redeem.py) · [Node.js](examples/redeem.mjs) · [Go](examples/redeem.go) · [PHP](examples/redeem.php)

这里的文件会随接口更新自动同步，以这里的最新版本为准。

## 接口地址

`https://recharge.chongplus.store/api/open/v1`

API 密钥（`cpa_` 开头、47 个字符）和来源 IP 登记找我们要。密钥只放在你的服务器上，不要提交到任何代码仓库。

每个请求都带上你自己系统的 `User-Agent`（比如 `my-shop/1.0`）：有些语言自带的默认值（Python 的 urllib、Java 8 等）会被网关直接拒绝，
回包是 HTTP 403 加一行纯文本 `error code: 1010`。详见在线文档「通用约定」。

## 四步完成一次兑换

1. `POST /cdk/query`：查卡密（可选）。
2. `POST /redeem/verify`：核对买家账号，拿到 `ticket`（约 10 秒）。
3. `POST /redeem`：用 `ticket` 加你生成的 `idempotency_key` 确认兑换，拿到 `order_no`。
4. 按 `poll_after` 秒轮询 `GET /orders/{order_no}`，直到 `succeeded` 或 `failed`。

网络超时、没拿到响应时，用同一个 `idempotency_key` 原样再发一次，不会重复兑换。完整的重试规则、状态处理和错误码见在线文档。

## 使用约定

- 本接口只提供兑换能力。买家看到的页面、文案和样式，由你自己设计、自己制作。
- 不得复制、仿制或套用我们网站的页面、样式、图片、视频和教程；不得用 iframe 嵌入我们的网站，也不得用自己的域名转发（镜像）我们的网站。
- 失败的订单只能由买家自己点重试，你的程序不得自动重新提交：每一次提交都会真实发起一次付款，同一个账号接连失败时，自动重交只会接着失败。
- 密钥只给你自己的系统用，不转借、不转卖。
- 订单结果以本接口返回的为准。
- 违反以上任何一条，我们会停用密钥。

## 运行示例

```bash
export AGENT_API_KEY=cpa_...
python examples/redeem.py <卡密> <买家 Session 文件>
node examples/redeem.mjs <卡密> <买家 Session 文件>
go run examples/redeem.go <卡密> <买家 Session 文件>
php examples/redeem.php <卡密> <买家 Session 文件>
```

## 更新记录

| 日期 | 内容 |
| --- | --- |
| 2026-10-08 | 新增 PHP 示例；新增「使用约定」；写明订单失败后不要让程序自动重新提交。接口本身没有变化。 |
| 2026-10-05 | v1 首版。 |

接口的兼容性变动我们会提前通知。
