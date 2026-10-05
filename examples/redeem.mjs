// 兑换接口 v1 示例（Node.js 18+，不用装任何依赖）。
//
// 用法：
//   export AGENT_API_KEY=cpa_...          # 密钥只放在服务器的环境变量里
//   node redeem.mjs <卡密> <买家 Session 文件>
//
// 把这份代码接进你的系统时：idempotency_key 要在调用 /redeem 之前和你自己的订单一起存进数据库，
// 重试时原样复用；买家的 Session 只在这一次请求里用，不要保存、不要写日志。

import { randomBytes } from 'node:crypto'
import { readFileSync } from 'node:fs'
import { setTimeout as sleep } from 'node:timers/promises'

const BASE = process.env.AGENT_API_BASE || 'https://recharge.chongplus.store/api/open/v1'
const KEY = process.env.AGENT_API_KEY
// 换成你自己系统的名字。别用语言自带的默认值：有的会被网关直接拒绝（HTTP 403，回包是 error code: 1010）。
const USER_AGENT = 'my-shop/1.0'

// 这几种错误说明「这次没处理」：等 retry_after 秒后，用同一张 ticket、同一个 idempotency_key 原样再发。
const RESEND_SAME = new Set([
  'busy',
  'rate_limited',
  'too_many_inflight',
  'service_unavailable',
  'service_disabled',
  'try_later',
  'internal_error',
])
const UNFINISHED = new Set(['processing', 'under_review'])

/** 接口明确返回了错误（ok=false）。 */
class ApiError extends Error {
  constructor(status, error, traceId) {
    super(`${error.code}: ${error.message} (trace_id=${traceId})`)
    this.status = status
    this.code = error.code || ''
    this.userMessage = error.message || '' // 给买家看的那句话
    this.retryAfter = error.retry_after || 0
    this.details = error.details || {}
  }
}

/** 没拿到响应（超时、断网、网关错误页）：结果未知，只能原样重发或按卡密找回。 */
class NetworkError extends Error {}

async function call(method, path, body) {
  let res
  try {
    res = await fetch(BASE + path, {
      method,
      headers: {
        Authorization: `Bearer ${KEY}`,
        'User-Agent': USER_AGENT,
        ...(body === undefined ? {} : { 'Content-Type': 'application/json' }),
      },
      body: body === undefined ? undefined : JSON.stringify(body),
      signal: AbortSignal.timeout(90_000), // 核对账号一步要 10 秒左右
    })
  } catch (e) {
    throw new NetworkError(String(e))
  }
  let payload
  try {
    payload = await res.json()
  } catch {
    throw new NetworkError(`HTTP ${res.status}，回包不是 JSON`)
  }
  if (payload.ok) return payload.data
  throw new ApiError(res.status, payload.error || {}, payload.trace_id)
}

/** 确认兑换。网络出错、或者是「这次没处理」的错误，都用同一张 ticket、同一个幂等号原样再发。 */
async function submit(ticket, idem, cdk) {
  for (let i = 0; i < 6; i++) {
    try {
      return await call('POST', '/redeem', { ticket, idempotency_key: idem })
    } catch (e) {
      if (e instanceof NetworkError) {
        await sleep(3000)
        continue
      }
      if (e.code === 'ticket_used' && e.details.order_no) {
        return call('GET', `/orders/${e.details.order_no}`) // 这张 ticket 已经提交过：查那张单
      }
      if (!RESEND_SAME.has(e.code)) throw e // 其他错误：这张 ticket 作废，处理完问题后从核对账号重新来，并换新的幂等号
      await sleep((e.retryAfter || 3) * 1000)
    }
  }
  // 一直没拿到明确结果：按卡密找回（可能已经受理了），绝不换新的幂等号重新提交。
  return call('POST', '/orders/lookup', { cdk })
}

async function waitUntilDone(order, deadlineMs = 2 * 3600_000) {
  const deadline = Date.now() + deadlineMs
  while (UNFINISHED.has(order.status)) {
    if (Date.now() > deadline) break // 等太久了：提示买家联系客服并给出 order_no，不要重新提交
    await sleep((order.poll_after || 8) * 1000)
    try {
      order = await call('GET', `/orders/${order.order_no}`)
    } catch (e) {
      if (!(e instanceof NetworkError) && !RESEND_SAME.has(e.code)) throw e
      await sleep((e.retryAfter || 10) * 1000)
    }
  }
  return order
}

export async function redeem(cdk, session) {
  const info = await call('POST', '/cdk/query', { cdk }) // 可选：先告诉买家是什么套餐
  console.error('套餐：', info.plan_name)

  const check = await call('POST', '/redeem/verify', { cdk, session })
  if (!check.eligible) return { ok: false, message: check.message } // 账号已有套餐：请买家换一个账号

  const idem = `ord-${randomBytes(12).toString('hex')}` // 实际使用时：和你的订单一起先存库，重试时原样复用
  const order = await waitUntilDone(await submit(check.ticket, idem, cdk))
  return {
    ok: order.status === 'succeeded',
    status: order.status,
    message: order.message,
    order_no: order.order_no,
    cdk_state: order.cdk_state,
    failure: order.failure,
  }
}

if (process.argv.length === 4) {
  const [cdk, sessionFile] = process.argv.slice(2)
  try {
    console.log(JSON.stringify(await redeem(cdk, readFileSync(sessionFile, 'utf8')), null, 2))
  } catch (e) {
    if (!(e instanceof ApiError)) throw e
    console.log(JSON.stringify({ ok: false, code: e.code, message: e.userMessage, details: e.details }, null, 2))
  }
} else {
  console.error('用法：node redeem.mjs <卡密> <买家 Session 文件>')
  process.exitCode = 1
}
