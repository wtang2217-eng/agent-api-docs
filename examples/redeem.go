// 兑换接口 v1 示例（Go 1.21+，只用标准库）。
//
// 用法：
//
//	export AGENT_API_KEY=cpa_...          # 密钥只放在服务器的环境变量里
//	go run redeem.go <卡密> <买家 Session 文件>
//
// 把这份代码接进你的系统时：idempotency_key 要在调用 /redeem 之前和你自己的订单一起存进数据库，
// 重试时原样复用；买家的 Session 只在这一次请求里用，不要保存、不要写日志。
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"time"
)

var (
	base   = envOr("AGENT_API_BASE", "https://recharge.chongplus.store/api/open/v1")
	key    = os.Getenv("AGENT_API_KEY")
	client = &http.Client{Timeout: 90 * time.Second} // 核对账号一步要 10 秒左右
)

// 换成你自己系统的名字。别用语言自带的默认值：有的会被网关直接拒绝（HTTP 403，回包是 error code: 1010）。
const userAgent = "my-shop/1.0"

// 这几种错误说明「这次没处理」：等 retry_after 秒后，用同一张 ticket、同一个 idempotency_key 原样再发。
var resendSame = map[string]bool{
	"busy": true, "rate_limited": true, "too_many_inflight": true, "service_unavailable": true,
	"service_disabled": true, "try_later": true, "internal_error": true,
}

// APIError 接口明确返回了错误（ok=false）。
type APIError struct {
	Status     int               `json:"-"`
	Code       string            `json:"code"`
	Message    string            `json:"message"`
	RetryAfter int               `json:"retry_after"`
	Details    map[string]string `json:"details"`
	TraceID    string            `json:"-"`
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s: %s (trace_id=%s)", e.Code, e.Message, e.TraceID)
}

// ErrNetwork 没拿到响应（超时、断网、网关错误页）：结果未知，只能原样重发或按卡密找回。
var ErrNetwork = errors.New("network error")

// Order 一张订单（只列了这里用到的字段）。
type Order struct {
	OrderNo   string `json:"order_no"`
	Status    string `json:"status"`
	CDKState  string `json:"cdk_state"`
	Message   string `json:"message"`
	PollAfter int    `json:"poll_after"`
	Failure   *struct {
		Code   string `json:"code"`
		Action string `json:"action"`
	} `json:"failure,omitempty"`
}

func call(method, path string, body, out any) error {
	var reader *bytes.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		reader = bytes.NewReader(raw)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, base+path, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("User-Agent", userAgent)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrNetwork, err)
	}
	defer resp.Body.Close()
	var payload struct {
		OK      bool            `json:"ok"`
		Data    json.RawMessage `json:"data"`
		Error   *APIError       `json:"error"`
		TraceID string          `json:"trace_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return fmt.Errorf("%w: HTTP %d，回包不是 JSON", ErrNetwork, resp.StatusCode)
	}
	if !payload.OK {
		e := payload.Error
		if e == nil {
			e = &APIError{Code: "internal_error"}
		}
		e.Status, e.TraceID = resp.StatusCode, payload.TraceID
		return e
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(payload.Data, out)
}

// submit 确认兑换。网络出错、或者是「这次没处理」的错误，都用同一张 ticket、同一个幂等号原样再发。
func submit(ticket, idem, cdk string) (*Order, error) {
	for i := 0; i < 6; i++ {
		var order Order
		err := call(http.MethodPost, "/redeem", map[string]string{"ticket": ticket, "idempotency_key": idem}, &order)
		if err == nil {
			return &order, nil
		}
		var apiErr *APIError
		switch {
		case errors.Is(err, ErrNetwork):
			time.Sleep(3 * time.Second)
		case errors.As(err, &apiErr) && apiErr.Code == "ticket_used" && apiErr.Details["order_no"] != "":
			// 这张 ticket 已经提交过：查那张单
			err = call(http.MethodGet, "/orders/"+apiErr.Details["order_no"], nil, &order)
			return &order, err
		case errors.As(err, &apiErr) && resendSame[apiErr.Code]:
			time.Sleep(time.Duration(max(apiErr.RetryAfter, 3)) * time.Second)
		default:
			return nil, err // 其他错误：这张 ticket 作废，处理完问题后从核对账号重新来，并换新的幂等号
		}
	}
	// 一直没拿到明确结果：按卡密找回（可能已经受理了），绝不换新的幂等号重新提交。
	var order Order
	err := call(http.MethodPost, "/orders/lookup", map[string]string{"cdk": cdk}, &order)
	return &order, err
}

func waitUntilDone(order *Order, deadline time.Duration) (*Order, error) {
	stop := time.Now().Add(deadline)
	for order.Status == "processing" || order.Status == "under_review" {
		if time.Now().After(stop) {
			break // 等太久了：提示买家联系客服并给出 order_no，不要重新提交
		}
		time.Sleep(time.Duration(max(order.PollAfter, 1)) * time.Second)
		var next Order
		err := call(http.MethodGet, "/orders/"+order.OrderNo, nil, &next)
		var apiErr *APIError
		switch {
		case err == nil:
			order = &next
		case errors.Is(err, ErrNetwork):
			time.Sleep(10 * time.Second)
		case errors.As(err, &apiErr) && resendSame[apiErr.Code]:
			time.Sleep(time.Duration(max(apiErr.RetryAfter, 10)) * time.Second)
		default:
			return order, err
		}
	}
	return order, nil
}

func redeem(cdk, session string) (map[string]any, error) {
	var info struct {
		PlanName string `json:"plan_name"`
	}
	if err := call(http.MethodPost, "/cdk/query", map[string]string{"cdk": cdk}, &info); err != nil {
		return nil, err
	}
	fmt.Fprintln(os.Stderr, "套餐：", info.PlanName) // 可选：先告诉买家是什么套餐

	var check struct {
		Eligible bool   `json:"eligible"`
		Ticket   string `json:"ticket"`
		Message  string `json:"message"`
	}
	if err := call(http.MethodPost, "/redeem/verify", map[string]string{"cdk": cdk, "session": session}, &check); err != nil {
		return nil, err
	}
	if !check.Eligible {
		return map[string]any{"ok": false, "message": check.Message}, nil // 账号已有套餐：请买家换一个账号
	}

	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	idem := "ord-" + hex.EncodeToString(buf) // 实际使用时：和你的订单一起先存库，重试时原样复用
	order, err := submit(check.Ticket, idem, cdk)
	if err != nil {
		return nil, err
	}
	if order, err = waitUntilDone(order, 2*time.Hour); err != nil {
		return nil, err
	}
	return map[string]any{
		"ok": order.Status == "succeeded", "status": order.Status, "message": order.Message,
		"order_no": order.OrderNo, "cdk_state": order.CDKState, "failure": order.Failure,
	}, nil
}

func envOr(name, fallback string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return fallback
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "用法：go run redeem.go <卡密> <买家 Session 文件>")
		os.Exit(1)
	}
	session, err := os.ReadFile(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	result, err := redeem(os.Args[1], string(session))
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		result = map[string]any{"ok": false, "code": apiErr.Code, "message": apiErr.Message, "details": apiErr.Details}
	} else if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	out, _ := json.MarshalIndent(result, "", "  ")
	fmt.Println(string(out))
}
