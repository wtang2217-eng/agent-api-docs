"""兑换接口 v1 示例（Python 3.8+，只用标准库）。

用法：
    export AGENT_API_KEY=cpa_...          # 密钥只放在服务器的环境变量里
    python redeem.py <卡密> <买家 Session 文件>

把这份代码接进你的系统时：idempotency_key 要在调用 /redeem 之前和你自己的订单一起存进数据库，
重试时原样复用；买家的 Session 只在这一次请求里用，不要保存、不要写日志。
"""

import http.client
import json
import os
import secrets
import sys
import time
import urllib.error
import urllib.request

BASE = os.environ.get("AGENT_API_BASE", "https://recharge.chongplus.store/api/open/v1")
KEY = os.environ["AGENT_API_KEY"]
# 换成你自己系统的名字。一定要带：urllib 的默认值（Python-urllib/3.x）会被网关直接拒绝（HTTP 403，回包是 error code: 1010）。
USER_AGENT = "my-shop/1.0"

# 这几种错误说明「这次没处理」：等 retry_after 秒后，用同一张 ticket、同一个 idempotency_key 原样再发。
RESEND_SAME = {"busy", "rate_limited", "too_many_inflight", "service_unavailable",
               "service_disabled", "try_later", "internal_error"}
UNFINISHED = {"processing", "under_review"}


class ApiError(Exception):
    """接口明确返回了错误（ok=false）。"""

    def __init__(self, status, error, trace_id):
        super().__init__(f"{error.get('code')}: {error.get('message')} (trace_id={trace_id})")
        self.status = status
        self.code = error.get("code", "")
        self.message = error.get("message", "")
        self.retry_after = error.get("retry_after") or 0
        self.details = error.get("details") or {}


class NetworkError(Exception):
    """没拿到响应（超时、断网、网关错误页）：结果未知，只能原样重发或按卡密找回。"""


def call(method, path, body=None):
    data = None if body is None else json.dumps(body, ensure_ascii=False).encode("utf-8")
    req = urllib.request.Request(BASE + path, data=data, method=method)
    req.add_header("Authorization", "Bearer " + KEY)
    req.add_header("User-Agent", USER_AGENT)
    if data is not None:
        req.add_header("Content-Type", "application/json")
    try:
        with urllib.request.urlopen(req, timeout=90) as resp:  # 核对账号一步要 10 秒左右
            status, raw = resp.status, resp.read()
    except urllib.error.HTTPError as e:  # 4xx / 5xx 也带着 JSON
        status, raw = e.code, e.read()
    except (OSError, http.client.HTTPException) as e:  # 超时、断网、连接被断开
        raise NetworkError(str(e)) from e
    try:
        payload = json.loads(raw)
    except ValueError as e:
        raise NetworkError(f"HTTP {status}，回包不是 JSON") from e
    if payload.get("ok"):
        return payload["data"]
    raise ApiError(status, payload.get("error") or {}, payload.get("trace_id"))


def submit(ticket, idem, cdk):
    """确认兑换。网络出错、或者是「这次没处理」的错误，都用同一张 ticket、同一个幂等号原样再发。"""
    for _ in range(6):
        try:
            return call("POST", "/redeem", {"ticket": ticket, "idempotency_key": idem})
        except NetworkError:
            time.sleep(3)
        except ApiError as e:
            if e.code == "ticket_used" and e.details.get("order_no"):
                return call("GET", "/orders/" + e.details["order_no"])  # 这张 ticket 已经提交过：查那张单
            if e.code not in RESEND_SAME:
                raise  # 其他错误：这张 ticket 作废，处理完问题后从核对账号重新来，并换新的幂等号
            time.sleep(e.retry_after or 3)
    # 一直没拿到明确结果：按卡密找回（可能已经受理了），绝不换新的幂等号重新提交。
    return call("POST", "/orders/lookup", {"cdk": cdk})


def wait_until_done(order, deadline_seconds=2 * 3600):
    deadline = time.monotonic() + deadline_seconds
    while order["status"] in UNFINISHED:
        if time.monotonic() > deadline:
            break  # 等太久了：提示买家联系客服并给出 order_no，不要重新提交
        time.sleep(order.get("poll_after") or 8)
        try:
            order = call("GET", "/orders/" + order["order_no"])
        except NetworkError:
            time.sleep(10)
        except ApiError as e:
            if e.code not in RESEND_SAME:
                raise
            time.sleep(e.retry_after or 10)
    return order


def redeem(cdk, session):
    info = call("POST", "/cdk/query", {"cdk": cdk})  # 可选：先告诉买家是什么套餐
    print("套餐：", info["plan_name"], file=sys.stderr)

    check = call("POST", "/redeem/verify", {"cdk": cdk, "session": session})
    if not check["eligible"]:
        return {"ok": False, "message": check["message"]}  # 账号已有套餐：请买家换一个账号

    idem = "ord-" + secrets.token_hex(12)  # 实际使用时：和你的订单一起先存库，重试时原样复用
    order = wait_until_done(submit(check["ticket"], idem, cdk))
    return {
        "ok": order["status"] == "succeeded",
        "status": order["status"],
        "message": order["message"],
        "order_no": order["order_no"],
        "cdk_state": order["cdk_state"],
        "failure": order.get("failure"),
    }


if __name__ == "__main__":
    if len(sys.argv) != 3:
        sys.exit("用法：python redeem.py <卡密> <买家 Session 文件>")
    with open(sys.argv[2], encoding="utf-8") as f:
        buyer_session = f.read()
    try:
        result = redeem(sys.argv[1], buyer_session)
    except ApiError as e:
        result = {"ok": False, "code": e.code, "message": e.message, "details": e.details}
    print(json.dumps(result, ensure_ascii=False, indent=2))
