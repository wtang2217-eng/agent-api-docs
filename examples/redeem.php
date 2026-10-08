<?php
// 兑换接口 v1 示例（PHP 7.4+，需要 curl 扩展）。
//
// 用法：
//   export AGENT_API_KEY=cpa_...          # 密钥只放在服务器的环境变量里
//   php redeem.php <卡密> <买家 Session 文件>
//
// 把这份代码接进你的系统时：idempotency_key 要在调用 /redeem 之前和你自己的订单一起存进数据库，
// 重试时原样复用；买家的 Session 只在这一次请求里用，不要保存、不要写日志。

define('API_BASE', getenv('AGENT_API_BASE') ?: 'https://recharge.chongplus.store/api/open/v1');
define('API_KEY', (string) getenv('AGENT_API_KEY'));
// 换成你自己系统的名字。别用框架或库自带的默认值：有的会被网关直接拒绝（HTTP 403，回包是 error code: 1010）。
const USER_AGENT = 'my-shop/1.0';

// 这几种错误说明「这次没处理」：等 retry_after 秒后，用同一张 ticket、同一个 idempotency_key 原样再发。
const RESEND_SAME = ['busy', 'rate_limited', 'too_many_inflight', 'service_unavailable', 'service_disabled', 'try_later', 'internal_error'];
const UNFINISHED = ['processing', 'under_review'];

/** 接口明确返回了错误（ok=false）。 */
class ApiError extends Exception
{
    public $status;
    public $errorCode;
    public $userMessage; // 给买家看的那句话
    public $retryAfter;
    public $details;

    public function __construct(int $status, array $error, string $traceId)
    {
        $this->status = $status;
        $this->errorCode = (string) ($error['code'] ?? '');
        $this->userMessage = (string) ($error['message'] ?? '');
        $this->retryAfter = (int) ($error['retry_after'] ?? 0);
        $this->details = is_array($error['details'] ?? null) ? $error['details'] : [];
        parent::__construct("{$this->errorCode}: {$this->userMessage} (trace_id={$traceId})");
    }
}

/** 没拿到响应（超时、断网、网关错误页）：结果未知，只能原样重发或按卡密找回。 */
class NetworkError extends Exception
{
}

function call(string $method, string $path, ?array $body = null): array
{
    $headers = ['Authorization: Bearer ' . API_KEY];
    $options = [
        CURLOPT_CUSTOMREQUEST => $method,
        CURLOPT_RETURNTRANSFER => true,
        CURLOPT_USERAGENT => USER_AGENT,
        CURLOPT_CONNECTTIMEOUT => 10,
        CURLOPT_TIMEOUT => 90, // 核对账号一步要 10 秒左右
    ];
    if ($body !== null) {
        $options[CURLOPT_POSTFIELDS] = json_encode($body, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES | JSON_THROW_ON_ERROR);
        $headers[] = 'Content-Type: application/json';
    }
    $options[CURLOPT_HTTPHEADER] = $headers;
    $ch = curl_init(API_BASE . $path);
    curl_setopt_array($ch, $options);
    $raw = curl_exec($ch);
    $status = (int) curl_getinfo($ch, CURLINFO_RESPONSE_CODE);
    if ($raw === false) {
        throw new NetworkError(curl_error($ch)); // 超时、断网、连接被断开
    }
    $payload = json_decode($raw, true);
    if (!is_array($payload)) {
        throw new NetworkError("HTTP {$status}，回包不是 JSON");
    }
    if (!empty($payload['ok'])) {
        return $payload['data'];
    }
    throw new ApiError($status, is_array($payload['error'] ?? null) ? $payload['error'] : [], (string) ($payload['trace_id'] ?? ''));
}

/** 确认兑换。网络出错、或者是「这次没处理」的错误，都用同一张 ticket、同一个幂等号原样再发。 */
function submit(string $ticket, string $idem, string $cdk): array
{
    for ($i = 0; $i < 6; $i++) {
        try {
            return call('POST', '/redeem', ['ticket' => $ticket, 'idempotency_key' => $idem]);
        } catch (NetworkError $e) {
            sleep(3);
        } catch (ApiError $e) {
            if ($e->errorCode === 'ticket_used' && !empty($e->details['order_no'])) {
                return call('GET', '/orders/' . rawurlencode($e->details['order_no'])); // 这张 ticket 已经提交过：查那张单
            }
            if (!in_array($e->errorCode, RESEND_SAME, true)) {
                throw $e; // 其他错误：这张 ticket 作废，处理完问题后从核对账号重新来，并换新的幂等号
            }
            sleep($e->retryAfter > 0 ? $e->retryAfter : 3);
        }
    }
    // 一直没拿到明确结果：按卡密找回（可能已经受理了），绝不换新的幂等号重新提交。
    return call('POST', '/orders/lookup', ['cdk' => $cdk]);
}

function waitUntilDone(array $order, int $deadlineSeconds = 7200): array
{
    $deadline = time() + $deadlineSeconds;
    while (in_array($order['status'], UNFINISHED, true)) {
        if (time() > $deadline) {
            break; // 等太久了：提示买家联系客服并给出 order_no，不要重新提交
        }
        sleep(max(1, (int) ($order['poll_after'] ?? 8)));
        try {
            $order = call('GET', '/orders/' . rawurlencode($order['order_no']));
        } catch (NetworkError $e) {
            sleep(10);
        } catch (ApiError $e) {
            if (!in_array($e->errorCode, RESEND_SAME, true)) {
                throw $e;
            }
            sleep($e->retryAfter > 0 ? $e->retryAfter : 10);
        }
    }
    return $order;
}

function redeem(string $cdk, string $session): array
{
    $info = call('POST', '/cdk/query', ['cdk' => $cdk]); // 可选：先告诉买家是什么套餐
    error_log('套餐：' . $info['plan_name']);

    $check = call('POST', '/redeem/verify', ['cdk' => $cdk, 'session' => $session]);
    if (empty($check['eligible'])) {
        return ['ok' => false, 'message' => $check['message']]; // 账号已有套餐：请买家换一个账号
    }

    $idem = 'ord-' . bin2hex(random_bytes(12)); // 实际使用时：和你的订单一起先存库，重试时原样复用
    $order = waitUntilDone(submit($check['ticket'], $idem, $cdk));
    return [
        'ok' => $order['status'] === 'succeeded',
        'status' => $order['status'],
        'message' => $order['message'],
        'order_no' => $order['order_no'],
        'cdk_state' => $order['cdk_state'],
        'failure' => $order['failure'] ?? null,
    ];
}

// 直接用命令行运行时才执行下面这段；被你的系统 include 进去时不会执行。
if (PHP_SAPI === 'cli' && realpath($_SERVER['SCRIPT_FILENAME'] ?? '') === __FILE__) {
    if ($argc !== 3) {
        fwrite(STDERR, "用法：php redeem.php <卡密> <买家 Session 文件>\n");
        exit(1);
    }
    $buyerSession = file_get_contents($argv[2]);
    if ($buyerSession === false) {
        exit(1);
    }
    try {
        $result = redeem($argv[1], $buyerSession);
    } catch (ApiError $e) {
        $result = ['ok' => false, 'code' => $e->errorCode, 'message' => $e->userMessage, 'details' => (object) $e->details];
    }
    echo json_encode($result, JSON_UNESCAPED_UNICODE | JSON_UNESCAPED_SLASHES | JSON_PRETTY_PRINT), PHP_EOL;
}
