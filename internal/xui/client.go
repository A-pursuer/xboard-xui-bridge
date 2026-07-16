package xui

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/xboard-bridge/xboard-xui-bridge/internal/config"
)

// Client 是 3x-ui 面板 API 客户端，仅支持 Bearer API Token 单通道。
//
// 鉴权机制（v0.6 起，仅适配 3x-ui v3.0.0+，详见 internal/config/config.go
// 文件头"3x-ui 鉴权说明"注释块）：
//
//	每次面板 API 请求都在请求头注入 "Authorization: Bearer <APIToken>"；
//	3x-ui v3.0.0 内 APIController.checkAPIAuth 走 SettingService.MatchApiToken
//	（constant-time compare）通道，校验通过后 c.Set("api_authed", true)，
//	让 CSRFMiddleware 立即短路——token 模式下连 mutating（POST）请求都
//	无需 X-CSRF-Token 头。
//
// 单一正向路径承诺：
//
//	a) 无登录、无 CSRF、无 cookie，请求路径只有"构造请求 → 注入 Bearer
//	   头 → 发送 → 解码 commonResp"四步；
//	b) 任何 4xx / 5xx / 网络错误都立即向上传播为 *Error，不在客户端层做
//	   隐式重试 / 重登录 / 兜底响应；
//	c) Token 错误（典型 401 / 404）= 运维在 3x-ui 面板重新生成 token 后
//	   未同步到本中间件——立即报错让运维显式介入。
//
// 并发安全：
//
//	a) httpClient 自带连接池天然线程安全；本 Client 不持有 cookiejar，
//	   也不需要登录串行化锁，apiToken 构造后只读，多 goroutine 并发请求无 race。
//	b) listMu 保护 /list 端点的周期内合并缓存（v0.5.3 起引入），仅保护
//	   短暂的 cache 状态读写，与请求路径无嵌套。
//	c) baseURL 由 atomic.Pointer 包装：v0.6.x 起当 3x-ui 面板切换 cert
//	   配置（操作员在面板侧改了 cert/key 路径、或 3x-ui 升级时 cert 路径
//	   被脚本清理）导致监听协议在 HTTP/HTTPS 之间漂移时，client 会在首
//	   次请求识别"协议不匹配"net/http 错误后做一次性 scheme 翻转 + 重试
//	   （详见 do 与 schemeSwapTarget 注释）；翻转通过 CAS 保证多 goroutine
//	   并发触发时只有第一个会真正改写 + 打 WARN，其余通过 CAS 失败回路
//	   继续走新 baseURL 重试，不会冲突。
type Client struct {
	// baseURL：host + base_path（不含尾 /）。v0.6.x 起允许在运行时被
	// schemeSwapTarget 一次性翻转 scheme（http↔https）；初始化后用 Load
	// 取快照，CAS 写以避免与并发请求的双向竞态。
	baseURL    atomic.Pointer[string]
	apiToken   string // Bearer 头注入值；构造后不可变
	httpClient *http.Client

	// logger：仅用于 scheme 自动翻转时的 WARN 提示。允许为 nil，此时
	// 退化为 slog.Default()；正式部署路径上由 supervisor.buildEngine 直接
	// 把 Supervisor.log 传进来，与其它 component 共享同一日志输出。
	logger *slog.Logger

	// /panel/api/inbounds/list 端点的周期内合并缓存（v0.5.3）。
	//
	// 背景：traffic_sync 与 alive_sync 都通过 GetClientTrafficsByInboundID
	// 间接拉 /list；多 Bridge 部署下每周期对同一 host 重复发相同请求。
	// 单周期内多次调用走缓存命中；多 goroutine 并发撞上过期 → singleflight
	// 合并成一次 HTTP，等待者通过 done channel 拿同一份结果。
	//
	// listCacheTTL 取 5 秒：远小于默认同步周期 60 秒，避免"上一周期数据
	// 跨周期使用"导致 user_sync 刚加的新 client 流量被推迟到下周期上报；
	// 同时仍能让 traffic_sync + alive_sync 在同一周期内（典型间隔 < 1s）
	// 共享一次拉取。
	//
	// 缓存仅作用于"已经鉴权通过的成功响应"，本身不绕过鉴权——任何
	// fetchInbounds 失败都不会污染 cache。
	listMu       sync.Mutex
	listCache    []Inbound          // 上次成功拉取的 inbounds 快照（nil 与空切片均可能为合法成功结果，例如 obj=null）
	listExpireAt time.Time          // 缓存有效性的真相源；零值或已过 = 缓存无效。listCache 自身不能用作"是否有缓存"的判定依据
	listInflight *listFetchInflight // 非 nil 表示有 goroutine 正在拉取，等待者订阅 done
}

// listCacheTTL 是 /list 端点缓存的有效期。详见 Client.listMu 字段注释。
const listCacheTTL = 5 * time.Second

// listFetchInflight 用于 singleflight：当一个 goroutine 正在拉 /list 时，
// 后续 caller 阻塞在 done channel 上，全部拿到同一份 result。
//
// done 关闭后，result 字段被视为"已经写入完毕"——所有 reader 都安全读取。
// 写入端必须在 close(done) 之前完成 result 的赋值；本约束由
// fetchInboundsCached 内部的代码顺序保证（写 result → close(done)）。
type listFetchInflight struct {
	done   chan struct{}
	result listFetchResult
}

// listFetchResult 是一次 /list 拉取的成功 / 失败状态二选一。
//
// 设计成简单结构体而非 (slice, error) 二元组：方便在 listFetchInflight 中
// 一次性赋值后让 close(done) 之后的读取者拿到一致快照，避免"slice 已写
// 但 err 还未写"的撕裂窗口。
type listFetchResult struct {
	inbounds []Inbound
	err      error
}

// New 构造 Client。
//
// 不发起任何网络请求——token 模式无需登录，请求注入仅在每次 do 时执行。
// 这让"半填配置"场景仍能完成 Client 构造，引擎后续 Reload 即可恢复。
//
// 构造失败原因仅可能源于 TLS 配置异常——但 *http.Transport 把 TLS 错误
// 推迟到首次请求时报告，本函数不主动检测。
//
// logger 允许为 nil（退化为 slog.Default()）；正式调用路径由
// supervisor.buildEngine 直接传 Supervisor.log，让 scheme 自动翻转的 WARN
// 与其它 component 日志走同一个 JSON handler。
func New(cfg config.Xui, logger *slog.Logger) (*Client, error) {
	transport := &http.Transport{
		// 不直接复用 http.DefaultTransport 的两个原因：
		//  1) SkipTLSVerify 必须按 cfg 决定；复用 default 会污染全局；
		//  2) default 的 ResponseHeaderTimeout=0（无限等响应头），3x-ui 在
		//     xray 重启 / inbound 重载窗口内可能延迟返回 header，单次请求
		//     会撑满整个 httpClient.Timeout（默认 15s）。本 transport 在
		//     IO 各阶段设独立超时，让"读 header 前卡死""TLS 握手卡死"
		//     "100-Continue 卡死"三类异常各自尽快返回；端到端总上限仍由
		//     httpClient.Timeout 兜底（含 DNS/TCP dial / 响应体读取阶段，
		//     本 transport 不再额外约束）。
		TLSClientConfig: &tls.Config{
			InsecureSkipVerify: cfg.SkipTLSVerify, //nolint:gosec // 由运维配置显式启用
			MinVersion:         tls.VersionTLS12,
		},
		// 连接池：MaxIdleConns 是全局闲置上限；MaxIdleConnsPerHost 与
		// MaxConnsPerHost 是按目标 host 细化限制——3x-ui 通常只有一个
		// host，但中间件在多 bridge 部署下会出现请求叠加：4 bridges × 4
		// 同步循环 = 16 基础并发，再加 alive_sync 内部 GetClientIPs 的
		// fan-out（每 bridge 信号量 8）——单周期最坏可达 4 × 8 = 32 个
		// 同时挂起的 GetClientIPs 请求。MaxConnsPerHost=32 给典型部署留
		// 头空间，避免单条卡住的请求把后续所有 RPC 排队等到
		// httpClient.Timeout（队列等待计入端到端超时会让本周期连环 fail）。
		// 显式锁定上限同时阻断"连接风暴 → ephemeral 端口耗尽"边角失败
		// （极端下新连接被 EADDRINUSE 拒绝）。
		MaxIdleConns:        16,
		MaxIdleConnsPerHost: 8,
		MaxConnsPerHost:     32,
		IdleConnTimeout:     90 * time.Second,
		// 细粒度超时：仅约束 TLS 握手 / 服务端读完 header / 100-Continue
		// 三个 IO 子阶段。10s / 10s / 1s 远高于正常 3x-ui 响应时间
		// （< 200ms），远低于默认 client.Timeout=15s。
		// 注意：这些超时不覆盖 DNS/TCP dial 与响应体读取——后两者仍由
		// httpClient.Timeout 端到端兜底。
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		// 显式启用 HTTP/2：当 Transport 设了自定义 TLSClientConfig 时，
		// Go net/http 会保守地禁用 HTTP/2 自动协商（除非显式置
		// ForceAttemptHTTP2=true），原因是 stdlib 担心调用方意图绕开
		// 默认 ALPN 行为。本中间件需要的是"自签证书放行"而非"禁用 h2"，
		// 故必须显式置 true 才能在 nginx + h2 反代场景下复用单 TCP 流，
		// 显著降低连接池压力（多个并发 RPC 共享一个 TCP）。
		ForceAttemptHTTP2: true,
	}

	httpClient := &http.Client{
		Transport: transport,
		Timeout:   time.Duration(cfg.TimeoutSec) * time.Second,
	}

	if logger == nil {
		logger = slog.Default()
	}
	c := &Client{
		apiToken:   cfg.APIToken,
		httpClient: httpClient,
		// 注入 module=xui 让所有 xui Client 日志带统一标签——v0.8.4 起项目
		// 各组件（supervisor / web / sync）都用 module 字段标识自己；之前
		// 由于 xui Client 只在 scheme 翻转时打一行 WARN，复用上游 logger
		// 没有 module 字段会让运维过滤"xui 模块的日志"困难。
		logger: logger.With("module", "xui"),
	}
	// 初始 baseURL 入字段前先把 scheme 规一化为小写：用户在配置层填入
	// "HTTPS://" 也合法（config.validateHTTPHost 借 url.Parse 做大小写不
	// 敏感判定后并未回写 lower-case），但后续 schemeSwapTarget 用
	// strings.HasPrefix 做精确比对。提前规一化避免"大写 scheme 让自动翻
	// 转沉默失效"边角失败。
	initial := normalizeSchemeLower(cfg.APIHost) + cfg.BasePath
	c.baseURL.Store(&initial)
	return c, nil
}

// normalizeSchemeLower 把 URL 字符串中的 scheme 段（"://" 前）转为小写，
// 保留其余字符原样。raw 不含 "://" 时原样返回（既包含空串，也兼容未来
// 异常路径，让函数无需调用方做前置非空判断）。
func normalizeSchemeLower(raw string) string {
	i := strings.Index(raw, "://")
	if i <= 0 {
		return raw
	}
	return strings.ToLower(raw[:i]) + raw[i:]
}

// GetInbound 调用 GET /panel/api/inbounds/get/:id 获取单个 inbound 详情。
//
// 返回值用途：本中间件仅在 user_sync 中使用其 RawSettings 字段（解析
// settings.clients[*] 做托管 client diff），不读 ClientStats。
//
// 重要陷阱（v0.5.1 文档化）：3x-ui 主线 service.GetInbound(id) 的实现仅
// 是 db.First(inbound, id)，**不调用 Preload("ClientStats") 也不调
// enrichClientStats**——所以本端点返回的 JSON 里 clientStats 字段是 nil
// 切片，UUID/SubID 也未被 enrich。如需"按 inbound 拉所有 client traffics"
// 应改用 GetClientTrafficsByInboundID（v0.5.1 起内部走 /list 端点）。
//
// 老版本（v0.5 之前）注释一度写"ClientStats 已被 enrichClientStats 填充"，
// 那是错的——只有 /list 端点对应的 GetInbounds(userId) / GetAllInbounds()
// 才 Preload + enrich，单条 GetInbound(id) 一直没这个待遇。user_sync 只
// 用 RawSettings 没暴露这个 bug；traffic_sync 与 alive_sync 之前调的是
// /getClientTrafficsById/:id（且把 inbound id 当 client UUID 传错），所以
// 也没踩到 GetInbound 的 ClientStats nil 陷阱——直到 v0.5.1 重新审视才发现。
func (c *Client) GetInbound(ctx context.Context, id int) (*Inbound, error) {
	endpoint := "/panel/api/inbounds/get/" + strconv.Itoa(id)
	raw, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	var ib Inbound
	if err := json.Unmarshal(raw, &ib); err != nil {
		return nil, fmt.Errorf("解码 %s 响应：%w", endpoint, err)
	}
	return &ib, nil
}

// ErrInboundNotFound 表示通过 GET /panel/api/inbounds/list 没找到目标 inbound。
//
// 调用方拿到本错误应理解为"3x-ui 端确实没有该 inbound"——可能根因是 inbound
// 被运维删除、token 模式下 SetAPIAuthUser(GetFirstUser) 拿到的账户不拥有
// 该 inbound（多用户面板下的隐性差异）、或中间件 bridge 配置中的 XuiInboundID
// 写错。返回明确 sentinel 错误而非 nil 切片，是项目"单一正向路径"承诺的具体
// 落实——v0.5.x 时代用"返回 nil 当作无 client"的兜底语义已被移除：找不到
// 就是找不到，调用方显式失败让运维立即可见。
var ErrInboundNotFound = errors.New("xui: /list 中未找到目标 inbound（请检查 inbound 是否被删除 / bridge 的 xui_inbound_id 是否正确 / token 对应的账户是否拥有该 inbound）")

// GetClientTrafficsByInboundID 拉取指定 inbound 下所有 client 的流量统计。
//
// v0.5.1 关键修复（v0.5 及之前一直误用，导致 Xboard 端流量永远 0）：
//
//	3x-ui 主线 GET /panel/api/inbounds/getClientTrafficsById/:id 的 :id
//	**实际上是 client UUID（即 inbound.settings.clients[*].id 字段，UUID
//	字符串）**，并不是 inbound id。3x-ui InboundService.GetClientTrafficByID
//	内部 SQL（v3 主线 web/service/inbound.go）是：
//
//	    SELECT ... FROM client_traffics WHERE email IN (
//	        SELECT JSON_EXTRACT(client.value, '$.email') AS email
//	        FROM inbounds, JSON_EACH(JSON_EXTRACT(inbounds.settings, '$.clients')) AS client
//	        WHERE JSON_EXTRACT(client.value, '$.id') in (?)
//	    )
//
//	把 inbound id（如整数 1）以字符串 "1" 传进去，会与 settings.clients[*].id
//	（UUID 字符串如 "550e8400-e29b-41d4-a716-446655440000"）做相等匹配——
//	**永远匹配不到任何 client**，端点返回空数组。表现为 traffic_sync 循环
//	zero 次进入、alive_sync myEmails 永远为空——直接造成 Xboard 用户 u/d
//	永远 0 与 alive 永远空 push 这两个综合症状。
//
// 正确做法（v0.5.1 切换至 /list 端点）：
//
//	调 GET /panel/api/inbounds/list 走 3x-ui InboundService.GetInbounds(userId)；
//	该方法用 db.Preload("ClientStats") 装载 HasMany 子记录，且后续 enrichClientStats
//	会把每个 ClientStats 的 UUID/SubID 字段从 settings.clients 里反查填充——
//	返回的每条 inbound.ClientStats 已经是"完全可用"状态。注意三件事：
//
//	  a) 不能用 GET /panel/api/inbounds/get/:id：3x-ui 主线 GetInbound(id)
//	     仅 db.First(inbound, id)，**不 Preload ClientStats**。GORM 的
//	     HasMany 关联仅靠 struct 标签不会自动加载子记录——必须显式 Preload。
//	     该端点返回的 inbound.clientStats 始终是 nil 切片（user_sync 只读
//	     RawSettings 没暴露这一陷阱，直到 v0.5.1 修复 traffic 流量 bug 时
//	     被 Codex 审查发现）。详见 GetInbound 注释。
//
//	  b) /list 走 GetInbounds(userId) 用 WHERE user_id = ? **严格过滤**——
//	     登录账户只能看到自己 user_id 下的 inbound。token 模式下 3x-ui v3.0.0
//	     APIController.checkAPIAuth 通过 MatchApiToken 后调用
//	     session.SetAPIAuthUser(c, GetFirstUser())——所以 token 模式实际看到
//	     的是"第一个用户"的 inbound 集合。多用户面板下若运维不是 user_id=1
//	     的管理员、或存在多个 admin 账户，目标 inbound 必须属于 GetFirstUser
//	     才能被 /list 看到。本函数拿到不到目标 inbound 时显式返回
//	     ErrInboundNotFound，配合 v0.6 移除 sync 层心跳兜底，运维在
//	     traffic_sync 失败日志里能立刻看到根因。
//
//	  c) 体积权衡：拉所有 inbound 比单条 inbound 多带 N-1 倍 settings JSON。
//	     3x-ui 一个面板典型 inbound 数 < 10、单 inbound client 数 < 数千，
//	     /list 完整 JSON 通常 < 数百 KB；30s 一次的 traffic_sync 周期完全
//	     可承受。如未来出现极端规模（万级 client），下个版本可换"先拉
//	     /get/:id 取 settings.clients[*].email，再批量调 /getClientTraffics/:email"
//	     的 N+1 路径——但当前阶段无此需求。
//
// 调用方契约（v0.6 起调整）：
//   - 找到目标 inbound 且其 ClientStats 非空 → 真实流量列表
//   - 找到目标 inbound 但 ClientStats 为空 → nil 切片。注意 3x-ui 在
//     AddInboundClient 时通常会一并创建对应的 client_traffics 行（即使
//     up/down 都是 0），所以正常运行下 ClientStats 不应为空；如果观察到
//     非 0 用户数 inbound 仍返回空 stats，更可能是 inbound 刚建尚无 client、
//     运维手动重置过 stats 表、或 /list 端 Preload 失败等异常情形。
//   - /list 响应中找不到目标 inbound id → 返回 ErrInboundNotFound（v0.6
//     起从"返回 nil 切片"改为显式错误；理由详见 ErrInboundNotFound 注释）。
//   - 网络 / 鉴权 / 5xx 错误 → 走 c.do 的标准错误链，向调用方传播
func (c *Client) GetClientTrafficsByInboundID(ctx context.Context, inboundID int) ([]ClientTraffic, error) {
	// v0.5.3 起改走 fetchInboundsCached：单 Client 实例下，多 Bridge 在同
	// 一同步周期内对同一 host 的多次调用合并为一次 HTTP，降低对 3x-ui
	// 面板的压力。详见 Client.listMu 字段与 fetchInboundsCached 注释。
	inbounds, err := c.fetchInboundsCached(ctx)
	if err != nil {
		return nil, err
	}
	for _, ib := range inbounds {
		if ib.ID == inboundID {
			// 找到目标 inbound：返回其 ClientStats（已经被 Preload + enrich）。
			// 切片可能 nil（无 client）——调用方按 nil/empty 处理即可。
			return ib.ClientStats, nil
		}
	}
	// /list 不含目标 inbound id：v0.6 起返回 ErrInboundNotFound，让上层
	// sync 循环在日志里显式打出该错误根因（不再被静默兜底为"无在线 / 无
	// 流量"）。
	return nil, fmt.Errorf("inbound_id=%d：%w", inboundID, ErrInboundNotFound)
}

// fetchInbounds 直接发 GET /panel/api/inbounds/list 并解码 obj 字段。
//
// 调用方一般通过 fetchInboundsCached 间接调本函数，由后者负责 TTL 控制
// 与 singleflight 合并。本函数本身无锁、无缓存——是 fetchInboundsCached
// 的纯 IO 子步骤。
//
// 返回值约定：
//
//	a) 服务端 obj=null 或 body 空 → 返回 (nil, nil)；
//	b) 解码成功 → 返回 inbounds 切片（可能为空切片）；
//	c) 网络 / 鉴权 / 5xx 错误 → 走 c.do 的标准错误链，向调用方传播。
func (c *Client) fetchInbounds(ctx context.Context) ([]Inbound, error) {
	endpoint := "/panel/api/inbounds/list"
	raw, err := c.do(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	// 3x-ui 在用户没有 inbound 时返回 obj=null。统一收口为"无可用数据"
	// 而非解码错误——这不是失败兜底，而是协议合法响应（obj=null 是
	// commonResp 在"该用户名下无 inbound"场景的正常输出形式）。
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var inbounds []Inbound
	if err := json.Unmarshal(raw, &inbounds); err != nil {
		return nil, fmt.Errorf("解码 %s 响应：%w", endpoint, err)
	}
	return inbounds, nil
}

// fetchInboundsCached 是带 TTL + singleflight 的 fetchInbounds 包装。
//
// 行为分支：
//
//	a) listCache 仍在 TTL 内 → 直接返回缓存切片（同一切片在 reader 间共享，
//	   调用方不应原地 mutate；当前调用点都是只读遍历，安全）；
//	b) 已有 goroutine 在拉（listInflight 非 nil）→ 阻塞在其 done channel
//	   上，等 fetcher 关闭后读取共享 result。等待期间响应 ctx 取消，避免
//	   引擎退出时被卡住；
//	c) 否则当前 goroutine 触发新一轮拉取：
//	    1) 在 listMu 内创建 inflight 句柄、赋给 c.listInflight；
//	    2) 释放 listMu 后调 fetchInbounds——不持锁，避免阻塞其它 caller；
//	    3) 拉完后重新拿 listMu，更新 cache（仅 err==nil 时）+ 清 inflight，
//	       最后写 inflight.result 并 close(done)。
//
// 失败语义：fetch 失败时 listCache / listExpireAt 都不更新——保持上一轮
// 成功结果在剩余 TTL 内仍可命中，下一个 caller 看到 expireAt 已过会触发
// 新一轮 fetch。这避免了"瞬时网络抖动让缓存被脏数据替换"。
//
// ctx 选择：fetcher 用本次"首个 caller"的 ctx。该 caller 提前取消会让
// fetch 失败、所有 waiter 一起失败——这与"同一引擎所有 worker 共用 ctx
// 树"的现实一致；引擎退出时所有 worker 也都将退出，无需独立 detach ctx。
//
// Panic 防护（v0.8.4 修复 Codex 审查指出的死锁风险）：fetcher 在 fetchInbounds
// 内部走标准库 net/http + json 路径，理论上不应 panic；但任意 nil pointer
// 解码 / 闭包变量异常等极端边角都可能 panic——若 panic 路径未清理
// c.listInflight + 未 close(done)，所有正在等待 done channel 的 caller 会
// **永久阻塞**直到进程被强杀；同时下一波 caller 看到 inflight 仍非 nil 也
// 会跟着卡。
//
// 处理策略：
//   a) defer 中清理 inflight 槽位（无条件，含 panic 路径）；
//   b) panic 时用 fmt.Errorf("xui.fetchInbounds panic: %v", rec) 包装成
//      普通 error 写入 inflight.result 并 close(done)——所有 waiter 看到
//      错误返回。该 error 不是 *xui.Error 类型（不携带 HTTPStatus / Endpoint
//      等结构化字段），因为 panic 信息只有 stack-time 的 recover 值，没有
//      请求层的元信息可填；上层 syncTraffic / syncAlive 只关心 err != nil
//      即报"本周期失败"，无需类型断言；
//   c) **不 re-panic**：本函数返回给调用方一个普通 error（含 panic 信息）。
//      v0.8.4 之前曾用 panic(rec) 上抛，但 runStep 此前无 recover，会让整个
//      Go 进程 crash；现 runStep 已加 recover，但此处仍走 error 返回更稳——
//      让 sync 层按"本周期同步失败 + warn 日志"正常处理，与其它 HTTP /
//      网络错误对齐，运维不需要区分"panic vs 网络抖动"两种异常路径。
//
// 并发不变量：c.listInflight 仅在持有 listMu 时被读 / 写；从 nil → 非 nil
// 与 非 nil → nil 都在 listMu 临界区内完成。fetcher 释放 listMu 期间其它
// goroutine 看到非 nil inflight 并订阅 done channel；fetcher 拿回 listMu
// 把 inflight 清空之后再 close(done)——任何已订阅的 waiter 都能正常醒来。
//
// Named return values（v0.8.4 起）：defer 中需要在 panic 路径修改返回的
// err / inbounds 让 caller 拿到 panic-as-error。Go 函数体 panic 中断后，
// defer 内对 unnamed return value 的修改不会被外层观察到——caller 会拿到
// 零值 (nil, nil) 误以为 fetch 成功且无 inbounds。必须用 named return。
func (c *Client) fetchInboundsCached(ctx context.Context) (inbounds []Inbound, err error) {
	c.listMu.Lock()
	if !c.listExpireAt.IsZero() && time.Now().Before(c.listExpireAt) {
		cached := c.listCache
		c.listMu.Unlock()
		return cached, nil
	}
	if c.listInflight != nil {
		inflight := c.listInflight
		c.listMu.Unlock()
		select {
		case <-inflight.done:
			return inflight.result.inbounds, inflight.result.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	// 当前 goroutine 触发拉取。
	inflight := &listFetchInflight{done: make(chan struct{})}
	c.listInflight = inflight
	c.listMu.Unlock()

	// finished 区分"fetchInbounds 已返回（无论成功失败）" vs "尚未开始 /
	// 中途 panic"。defer 用它决定是否更新 cache。
	finished := false
	defer func() {
		// 顺序：先 recover 把 panic 转成 err（影响 cache 判断），再持锁
		// 清理 inflight 槽位 + 按需更新 cache，最后写 result + close(done)。
		//
		// 旧实现把 recover 放在最后一步，但那样 cache 路径基于"未 recover
		// 前的初始 err=nil"判定，panic 路径下 inbounds 可能是部分赋值的
		// 垃圾数据，cache 判定要靠 finished 二次防御才不出错。把 recover
		// 提前让 err 在 cache 判定时已是最终态，逻辑更直观。
		if rec := recover(); rec != nil {
			err = fmt.Errorf("xui.fetchInbounds panic: %v", rec)
			inbounds = nil
			finished = false
		}

		c.listMu.Lock()
		if err == nil && finished {
			// 仅在 fetch 真正成功完成时才更新 cache。panic / 失败路径
			// 都保留旧 cache 在剩余 TTL 内继续生效。
			c.listCache = inbounds
			c.listExpireAt = time.Now().Add(listCacheTTL)
		}
		c.listInflight = nil
		c.listMu.Unlock()

		// 写最终 result + 关 done channel。写必须在 close 之前完成，
		// 否则 reader 看到 done 已 close 但 result 还是零值。
		inflight.result = listFetchResult{inbounds: inbounds, err: err}
		close(inflight.done)
	}()

	inbounds, err = c.fetchInbounds(ctx)
	finished = true
	return inbounds, err
}

// AddClient 调用 POST /panel/api/clients/bulkCreate 批量新增 client。
//
// v3.3+ 路径：旧 /panel/api/inbounds/addClient 端点已被移除，新路径下 body
// 不再是 inbound.settings JSON 字符串，而是数组形态——每项独立携带 client
// 完整对象 + 它要挂载到的 inboundIds。bridge 每个 worker 只管一个 inbound，
// 所以所有项的 InboundIds 都是 []int{inboundID}。
//
// 入参 clients 是 bridge 协议适配器产出的内部 ClientSettings 切片；本函数
// 在序列化前把它们转成 wire 形态 ClientPayload，避免协议层与 wire 层耦合。
//
// 服务端 fillProtocolDefaults 会对 ID / Password / Auth 中的空字段按 inbound
// protocol 自动补充随机值（详见 3x-ui client_crud.go:125-146），所以
// trojan/shadowsocks/hysteria/hysteria2 这几类协议若 bridge 传 password/auth
// 为空，服务端会替我们补；当前 bridge 协议适配器都已显式填好相应字段。
//
// 关键陷阱（v0.8.3 Codex 审核指出）：bulkCreate 即使有 client 失败
// （典型如 "email already in use" / "subId already in use" / 目标 inbound
// 不存在）也会在顶层返回 success=true；失败信息全部在 obj.skipped 数组里。
// 本函数必须解码 obj 检查 Skipped 长度与 Created 计数——任何一个 skip 都
// 立即上抛错误（含 email + 服务端给出的原因），否则 sync 层会按"成功"写
// baseline 导致下周期 diff 看不到该用户、永久不再尝试 add（典型表现：少
// 数用户在 Xboard 一直存在但 3x-ui 上始终缺位）。
func (c *Client) AddClient(ctx context.Context, inboundID int, clients []ClientSettings) error {
	if len(clients) == 0 {
		return nil
	}
	body := make([]BulkCreateItem, 0, len(clients))
	for _, s := range clients {
		body = append(body, BulkCreateItem{
			Client:     clientSettingsToPayload(s),
			InboundIds: []int{inboundID},
		})
	}
	endpoint := "/panel/api/clients/bulkCreate"
	raw, err := c.do(ctx, http.MethodPost, endpoint, body)
	if err != nil {
		return err
	}
	// 顶层 success=true 不代表全部入库成功：解码 obj 检查 Skipped。
	// obj 在所有 client 都成功时形态为 `{"created":N,"skipped":null}` 或
	// 省略 skipped 字段；len(raw)==0 / "null" 视作"无 obj" 不安全——服务端
	// 这条路径总是返回结构化 obj，缺失 obj 反而说明协议异常。
	if len(raw) == 0 || string(raw) == "null" {
		return fmt.Errorf("bulkCreate：响应 obj 为空，无法判定 created/skipped 状态（疑似 3x-ui 协议异常）")
	}
	var result BulkCreateResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return fmt.Errorf("bulkCreate：解码 obj 失败：%w", err)
	}
	if len(result.Skipped) > 0 {
		// 至少一项失败：拼成单行错误向上抛。把所有 skipped 都列出来，避免
		// 多 client 批量场景下只看到第一个的运维盲区。
		details := make([]string, 0, len(result.Skipped))
		for _, sk := range result.Skipped {
			details = append(details, fmt.Sprintf("%s=%s", sk.Email, sk.Reason))
		}
		return fmt.Errorf("bulkCreate：created=%d/%d，skipped %d 项 [%s]",
			result.Created, len(clients), len(result.Skipped), strings.Join(details, "; "))
	}
	if result.Created != len(clients) {
		// 防御性检查：理论上 len(Skipped)==0 时 Created 必然 == len(clients)；
		// 若不等说明 3x-ui 内部统计逻辑异常，宁可报错让运维介入也别静默放过。
		return fmt.Errorf("bulkCreate：created=%d 但请求了 %d，skipped 为空，疑似 3x-ui 统计异常",
			result.Created, len(clients))
	}
	return nil
}

// UpdateClient 调用 POST /panel/api/clients/update/:email?inboundIds=<id>。
//
// v3.3+ 路径：旧 /panel/api/inbounds/updateClient/:clientKey 端点已被移除；
// 新路径下 URL 主键统一为 email，body 直接是 ClientPayload（不再嵌
// settings.clients[]）。inboundIds 通过 query 限定作用域——同一 email 若被
// 关联到多个 inbound（多 bridge 共享 panel 场景下不会出现，但 3x-ui 支持
// 跨 inbound 共享 client），bridge 应当只改自己管理的那条关联。
func (c *Client) UpdateClient(ctx context.Context, inboundID int, client ClientSettings) error {
	if client.Email == "" {
		return fmt.Errorf("updateClient：client.Email 不能为空（新版 API 按 email 路由）")
	}
	endpoint := "/panel/api/clients/update/" + url.PathEscape(client.Email) +
		"?inboundIds=" + url.QueryEscape(strconv.Itoa(inboundID))
	body := clientSettingsToPayload(client)
	if _, err := c.do(ctx, http.MethodPost, endpoint, body); err != nil {
		return err
	}
	return nil
}

// DelClientByEmail 调用 POST /panel/api/clients/del/:email 硬删除 client。
//
// v3.3+ 路径：旧 /panel/api/inbounds/:id/delClientByEmail/:email 端点已被移除；
// 新版 /del/:email 删除范围是"该 email 对应的 ClientRecord + 关联的所有
// inbound + traffic + IP 历史"，比旧版"仅从指定 inbound 移除"语义更宽。
//
// 签名变化（v0.8.3 起）：丢掉旧版的 inboundID 入参——新端点不按 inbound
// 限定，把它留在签名里只会让调用方误以为还能控制作用域。调用方
// （internal/sync/user_sync.go applyDelete）已同步更新。
//
// bridge 为何选硬删（/del）而不选按 inbound 解关联（/:email/detach）：
//
//	a) MakeEmail 生成的 email 含 node_id 后缀，跨 bridge 不冲突——单台
//	   3x-ui 上不存在"同一 email 被多个 bridge 各自的 inbound 引用"的合
//	   法场景；
//	b) 选 detach 会留下孤儿 ClientRecord，下次同 UUID 的用户回来时
//	   bulkCreate 会按 email 查到旧 ClientRecord 并检测 SubID 一致性失败
//	   （client_crud.go:74-84），导致每周期重复 "email already in use"
//	   错误；
//	c) 硬删一并清掉 traffic / IP 历史与本 bridge 的"用户已离开本节点"
//	   语义一致。
//
// 即使该 email 在 3x-ui 中已不存在，服务端会返回 success=false + msg=
// "client not found in any inbound or client record"。当前实现把它作为
// 错误向上传播——避免静默吞掉异常状态；同步 baseline 与 3x-ui 真实状态
// 偏离时让运维一眼可见。
func (c *Client) DelClientByEmail(ctx context.Context, email string) error {
	endpoint := "/panel/api/clients/del/" + url.PathEscape(email)
	if _, err := c.do(ctx, http.MethodPost, endpoint, nil); err != nil {
		return err
	}
	return nil
}

// clientSettingsToPayload 把 bridge 内部 ClientSettings 转成 v3.3+ wire 形态
// ClientPayload。两者字段几乎一一对应，仅命名 Method→Security（v3.3 JSON 键
// 改名）以及枚举 wire 必填字段而已。
//
// 拆出独立函数（而非让 ClientSettings 直接实现 MarshalJSON 自变形）：
//
//	a) 让 ClientSettings 保持纯数据语义、可被 diff 比较，避免 marshal 路径
//	   引入 wire-version 选择副作用；
//	b) 协议适配器层只需感知 ClientSettings；wire 转换内聚在 xui 包内。
func clientSettingsToPayload(s ClientSettings) ClientPayload {
	return ClientPayload{
		ID:         s.ID,
		Security:   s.Security,
		Password:   s.Password,
		Flow:       s.Flow,
		Auth:       s.Auth,
		Email:      s.Email,
		LimitIP:    s.LimitIP,
		TotalGB:    s.TotalGB,
		ExpiryTime: s.ExpiryTime,
		Enable:     s.Enable,
		SubID:      s.SubID,
		Reset:      s.Reset,
	}
}

// GetClientIPs 调用 POST /panel/api/clients/ips/:email，
// 返回该 client 当前记录过的 IP 列表。
//
// v3.3+ 把端点从 /panel/api/inbounds/clientIps/:email 搬到
// /panel/api/clients/ips/:email。v3.5 前后该端点又从字符串数组扩展为
// ClientIpInfo 对象数组（含 ip/time/node），本函数统一归一化为 []string。
//
// 中间件主要用 GetOnlines() 拿在线 email，再拿 GetClientIPs 找该 email 当下使用的 IP。
// 对旧版字符串数组，时间戳信息保留在原始字符串中
// （"1.2.3.4 (2024-01-02 15:04:05)"），调用方自行解析；对新版对象数组，
// 只取 ip 字段，time/node 仅供 3x-ui UI 展示，alive 上报不需要。
//
// 协议合法响应形态（仅以下形态被视为成功，全部映射为 (nil, nil) 或数组）：
//
//	a) obj=null 或空 body                  → (nil, nil)（IP 表无该 email 行）
//	b) obj 为字符串 "No IP Record"          → (nil, nil)（3x-ui 主线 controller
//	                                          在 inboundService.GetInboundClientIps
//	                                          返回空 / err 时显式输出该字面值）
//	c) obj 为 JSON 数组（可空）              → 元素可为旧版字符串或新版 {ip,time,node}
//
// 任何其它形态（数字 / 对象 / 其它字符串等）一律视为协议错误向上传播——
// 这不是失败兜底，是"无数据 vs 协议异常"语义边界的显式拦截，避免静默
// 把异常响应映射成空切片导致 alive_sync 少报用户。
func (c *Client) GetClientIPs(ctx context.Context, email string) ([]string, error) {
	endpoint := "/panel/api/clients/ips/" + url.PathEscape(email)
	raw, err := c.do(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	// 数组形态：兼容旧版字符串元素，以及新版 ClientIpInfo 对象元素。
	if raw[0] == '[' {
		out, err := decodeClientIPEntries(raw)
		if err != nil {
			return nil, fmt.Errorf("解码 %s 响应：%w", endpoint, err)
		}
		return out, nil
	}
	// 字符串形态：仅接受 "No IP Record" 字面值；其它字符串一律报错。
	// 不接受任何其它字符串字面值（如未来 fork 改文案）：让协议变化时立刻
	// 显式失败，由维护者更新本判断而不是静默吞掉，与"单一正向路径"承诺
	// 一致。
	if raw[0] == '"' {
		var msg string
		if err := json.Unmarshal(raw, &msg); err != nil {
			return nil, fmt.Errorf("解码 %s 响应（字符串形态）：%w", endpoint, err)
		}
		if msg == "No IP Record" {
			return nil, nil
		}
		return nil, fmt.Errorf("解码 %s 响应：obj 为意外字符串 %q（仅接受数组 / null / \"No IP Record\"）", endpoint, msg)
	}
	// 数字 / 对象 / 布尔等其它类型：协议异常，立即报错。
	return nil, fmt.Errorf("解码 %s 响应：obj 既非数组也非 \"No IP Record\"：%s", endpoint, truncate(string(raw), 128))
}

func decodeClientIPEntries(raw json.RawMessage) ([]string, error) {
	var entries []json.RawMessage
	if err := json.Unmarshal(raw, &entries); err != nil {
		return nil, err
	}
	out := make([]string, 0, len(entries))
	for i, entry := range entries {
		entry = bytes.TrimSpace(entry)
		if len(entry) == 0 {
			return nil, fmt.Errorf("第 %d 项为空", i)
		}
		switch entry[0] {
		case '"':
			var ip string
			if err := json.Unmarshal(entry, &ip); err != nil {
				return nil, fmt.Errorf("第 %d 项字符串解码失败：%w", i, err)
			}
			out = append(out, ip)
		case '{':
			ip, ok, err := decodeClientIPObject(entry)
			if err != nil {
				return nil, fmt.Errorf("第 %d 项对象 ip 字段无效：%w", i, err)
			}
			if !ok {
				return nil, fmt.Errorf("第 %d 项对象缺少 ip 字段", i)
			}
			if ip != "" {
				out = append(out, ip)
			}
		default:
			return nil, fmt.Errorf("第 %d 项既非字符串也非对象：%s", i, truncate(string(entry), 128))
		}
	}
	return out, nil
}

func decodeClientIPObject(raw json.RawMessage) (string, bool, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return "", false, err
	}
	field, ok := obj["ip"]
	if !ok {
		return "", false, nil
	}
	var ip string
	if err := json.Unmarshal(field, &ip); err != nil {
		return "", true, err
	}
	return ip, true, nil
}

// GetOnlines 调用 POST /panel/api/clients/onlines；返回当前在线的 email 列表。
//
// 该数据是面板从本地 + 远程节点聚合得到，刷新周期约 10 秒。
// v3.3+ 把端点从 /panel/api/inbounds/onlines 搬到 /panel/api/clients/onlines，
// 响应形态不变（commonResp.obj 是 email 字符串数组）。
func (c *Client) GetOnlines(ctx context.Context) ([]string, error) {
	endpoint := "/panel/api/clients/onlines"
	raw, err := c.do(ctx, http.MethodPost, endpoint, nil)
	if err != nil {
		return nil, err
	}
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var out []string
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("解码 %s 响应：%w", endpoint, err)
	}
	return out, nil
}

// GetServerStatus 调用 GET /panel/api/server/status；返回 cpu / mem / netUp / netDown 等。
//
// 用于把节点负载上报回 Xboard。返回值为 RawMessage，由调用方按需解构——本包不
// 提前定义结构是为了避免 3x-ui 字段调整时本包跟着改版。
func (c *Client) GetServerStatus(ctx context.Context) (json.RawMessage, error) {
	return c.do(ctx, http.MethodGet, "/panel/api/server/status", nil)
}

// do 是所有面板 API 共享的传输层。
//
// 正向路径（与项目"单一正向路径"承诺一致）：
//
//	1. 序列化 body（如有）；
//	2. 调 doOnce 用当前 baseURL 走"构造请求 → 注入 Bearer → 发 → 解码"四步；
//	3. 成功立即返回；
//	4. 仅当失败错误形态被 schemeSwapTarget 识别为"3x-ui 面板侧协议与
//	   xui.api_host 配置的 scheme 不匹配"时（即 Go net/http 两条标志性
//	   transport 错误），CAS 翻转 baseURL scheme 一次后重试一次。
//
// 为什么在此层做协议探测重试（而非更严格的"出错即失败"）：
//
//	a) 3x-ui 在 cert/key 路径无效或 LoadX509KeyPair 失败时静默退回 HTTP
//	   监听（web.go:415-430），运维侧改 cert / 升级脚本误删 cert 都会让
//	   面板从 HTTPS 退回 HTTP（反向则是 cert 配置成功 / 续期成功）；
//	b) 此类"协议漂移"属于面板部署事实，不属于运行期临时性瞬时失败，
//	   做一次性翻转后续请求全部走新 scheme，不构成"隐式重试 / 重登录"
//	   的复活兜底语义；
//	c) 翻转仅触发于两条 Go 标准库标志性 transport 错误字符串——4xx / 5xx
//	   / success=false / 任何 HTTP 层语义错误都不触发翻转。
//
// 翻转语义：
//
//	1. 首次失败立即识别错误形态，无指数退避、无多轮 sniff；
//	2. CAS 写：多 goroutine 并发触发时只有第一个真正改写 baseURL 并打
//	   WARN，其它通过 CAS 失败回路直接走新 scheme 重试；
//	3. 翻转后的重试仍然只算一次——若新 scheme 上仍报错（任何错误，无论
//	   是否再匹配 schemeSwapTarget 条件）立即将该错误向上传播，不会出
//	   现"http → https → http → ..." 反复抖动。
func (c *Client) do(ctx context.Context, method, endpoint string, body any) (json.RawMessage, error) {
	var bodyBytes []byte
	if body != nil {
		var err error
		bodyBytes, err = json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("序列化 %s body：%w", endpoint, err)
		}
	}

	baseSnap := c.baseURL.Load()
	raw, err := c.doOnce(ctx, method, endpoint, bodyBytes, body != nil, *baseSnap)
	if err == nil {
		return raw, nil
	}

	newBase, ok := schemeSwapTarget(*baseSnap, err)
	if !ok {
		return nil, err
	}

	// CAS：只有第一个翻转者真正改写 baseURL，其余 goroutine 看到 baseSnap
	// 已被替换，CAS 失败 → 不打 WARN，直接走新 scheme 重试。这样并发触发
	// 时日志里只有一行 "scheme auto-switched"，避免噪音。
	newPtr := &newBase
	retryBase := newBase
	if c.baseURL.CompareAndSwap(baseSnap, newPtr) {
		// module=xui 已经在 New 中通过 c.logger = logger.With(...) 注入，
		// 此处不再重复，避免 JSON 输出出现两个同名键。
		c.logger.Warn("3x-ui 面板侧协议与 xui.api_host 配置不一致，自动翻转 scheme 后重试",
			"event", "xui_scheme_autoswitch",
			"old_base", *baseSnap,
			"new_base", newBase,
			"endpoint", endpoint,
			"trigger_err", err.Error(),
			"hint", "运维侧建议把 xui.api_host 同步改为新的协议头，避免下次重启再次踩到首次失败",
		)
	} else {
		// CAS 失败说明另一个 goroutine 已经写过 baseURL：通常它写的是与
		// 本地 newBase 完全相同的值（同一段 baseSnap → schemeSwapTarget
		// 推导是确定性的），但极端的 "A→B→A 双向反复翻转" 路径下当前
		// 字段里的值可能已是 A（与本地 newBase 不同）。重新 Load 取真相
		// 源保险——避免本次 retry 用陈旧本地推导覆盖新的真实状态。
		retryBase = *c.baseURL.Load()
	}

	// 重试一次。无论结果如何都向上返回，不再做第二次翻转——避免在
	// http/https 之间反复抖动。
	return c.doOnce(ctx, method, endpoint, bodyBytes, body != nil, retryBase)
}

// doOnce 单次请求 + 解码；不做任何重试 / 翻转。
//
// 拆出独立函数是为了让 do 的"首试 → 识别 → CAS → 重试"主干清晰；
// 失败统一抽象为 *Error（HTTPStatus=0 表示传输层错误），便于
// schemeSwapTarget 通过类型断言而非字符串通配匹配。
func (c *Client) doOnce(ctx context.Context, method, endpoint string, bodyBytes []byte, hasBody bool, base string) (json.RawMessage, error) {
	full := base + endpoint

	var bodyReader io.Reader
	if hasBody {
		bodyReader = bytes.NewReader(bodyBytes)
	}

	req, err := http.NewRequestWithContext(ctx, method, full, bodyReader)
	if err != nil {
		return nil, fmt.Errorf("构造 %s 请求：%w", endpoint, err)
	}
	if hasBody {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	// Bearer 头：每次请求都注入；3x-ui v3.0.0 APIController.checkAPIAuth
	// 在该头部前缀匹配 "Bearer " 后调 SettingService.MatchApiToken（constant
	// time compare），通过即 c.Set("api_authed", true)，CSRFMiddleware 短路。
	req.Header.Set("Authorization", "Bearer "+c.apiToken)

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, &Error{HTTPStatus: 0, Endpoint: endpoint, Msg: err.Error()}
	}
	defer drainAndClose(resp.Body)

	rawBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024*1024)) // 4MB 兜底，避免巨型响应耗内存

	if resp.StatusCode/100 != 2 {
		return nil, &Error{
			HTTPStatus: resp.StatusCode,
			Endpoint:   endpoint,
			Msg:        sniffMsg(rawBody),
			RawBody:    truncate(string(rawBody), 512),
		}
	}

	// 解析 commonResp。3x-ui 全量端点都遵循该外壳。
	var cr commonResp
	if err := json.Unmarshal(rawBody, &cr); err != nil {
		return nil, &Error{
			HTTPStatus: resp.StatusCode,
			Endpoint:   endpoint,
			Msg:        "响应非 JSON",
			RawBody:    truncate(string(rawBody), 512),
		}
	}
	if !cr.Success {
		return nil, &Error{
			HTTPStatus: resp.StatusCode,
			Endpoint:   endpoint,
			Msg:        cr.Msg,
			RawBody:    truncate(string(rawBody), 512),
		}
	}
	return cr.Obj, nil
}

// schemeMismatchHTTPSToHTTP 是 Go 标准库 net/http 在"客户端用 HTTPS 连接
// 纯 HTTP 服务端"时返回的固定子串。出现该串说明本端 baseURL 是 https://
// 但 3x-ui 实际监听是 HTTP——直接翻转为 http:// 重试。
const schemeMismatchHTTPSToHTTP = "server gave HTTP response to HTTPS client"

// tlsHandshakeRecordPrefixes 列出"被 HTTP 客户端误读到响应头但首字节其实
// 是 TLS 记录"时，Go net/textproto 用 %q 输出的合法前缀。出现以下任一前
// 缀均强烈指向"http:// 误连接 HTTPS 服务端"。
//
// 字节含义（TLS 1.0+ RFC 5246/8446 ContentType + ProtocolVersion 高字节）：
//
//	\x14\x03 = ChangeCipherSpec, TLS 1.x
//	\x15\x03 = Alert, TLS 1.x（server 拒绝 plain-HTTP 请求时常见的回包）
//	\x16\x03 = Handshake (典型 ServerHello), TLS 1.x
//	\x17\x03 = ApplicationData, TLS 1.x
//
// 选这四个字节而非更宽的 `malformed HTTP response` 子串匹配，避免坏代理 /
// SSH banner / 错端口的 SMTP/POP3 greeting / MITM 乱字节等误把"非 scheme
// drift"映射成 scheme 翻转。Codex 审核 (2026-06-13) 明确要求收紧该判定。
var tlsHandshakeRecordPrefixes = []string{
	`malformed HTTP response "\x14\x03`,
	`malformed HTTP response "\x15\x03`,
	`malformed HTTP response "\x16\x03`,
	`malformed HTTP response "\x17\x03`,
}

// looksLikeTLSRecordOnHTTP 判定 msg 是否带任一 TLS record 特征前缀。
// 任一命中即返回 true；零命中返回 false。供 schemeSwapTarget 使用。
func looksLikeTLSRecordOnHTTP(msg string) bool {
	for _, p := range tlsHandshakeRecordPrefixes {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

// schemeSwapTarget 判定当前 base 与 err 是否构成"协议不匹配"语义；
// 若是，返回翻转 scheme 后的新 base 与 true，调用方据此 CAS 写 + 重试。
//
// 返回 (newBase, true) 的条件（两两必须同时满足）：
//
//	a) err 是本包 *Error 且 HTTPStatus==0（即传输层错误，不是 HTTP 响应
//	   层的 4xx/5xx——后者意味着 server 已正常应答 HTTP，scheme 必然匹
//	   配，纯粹业务 / 鉴权问题应继续传播让运维介入）；
//	b) err.Msg 命中 schemeMismatchHTTPSToHTTP 且 base 以 https:// 开头；
//	   或命中 looksLikeTLSRecordOnHTTP 且 base 以 http:// 开头。
//
// 注意 3x-ui 在 HTTP→HTTPS 端口上更常见的服务端反应是直接 RST / EOF，
// 此时 client 拿到的会是 "connection reset" / "EOF" 类错误，本中间件不
// 在该类无歧义错误上做翻转——它们也可能是网络层瞬时抖动，盲翻转会让
// "短暂掉线"被错误地映射成"协议变了"造成不必要的状态切换。
//
// 其它任何错误（DNS 解析失败 / connection refused / timeout / EOF /
// TLS 证书校验失败 / commonResp.success=false 等）一律不翻转，向上原样
// 传播——这与项目"单一正向路径"承诺保持一致：scheme 翻转只针对"3x-ui
// 面板端协议监听漂移"这一个明确根因，不做泛化的连通性试探。
func schemeSwapTarget(base string, err error) (string, bool) {
	var xe *Error
	if !errors.As(err, &xe) || xe.HTTPStatus != 0 {
		return "", false
	}
	msg := xe.Msg
	switch {
	case strings.HasPrefix(base, "https://") && strings.Contains(msg, schemeMismatchHTTPSToHTTP):
		return "http://" + strings.TrimPrefix(base, "https://"), true
	case strings.HasPrefix(base, "http://") && looksLikeTLSRecordOnHTTP(msg):
		return "https://" + strings.TrimPrefix(base, "http://"), true
	}
	return "", false
}

// truncate 把字符串截到最多 max 字节，超长部分用 "…" 标记，便于日志阅读。
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// sniffMsg 在错误响应中尽力提取 commonResp.msg；失败时返回原始 body 的前 64 字节。
func sniffMsg(raw []byte) string {
	if len(raw) == 0 {
		return ""
	}
	var probe struct {
		Msg string `json:"msg"`
	}
	if err := json.Unmarshal(raw, &probe); err == nil && probe.Msg != "" {
		return probe.Msg
	}
	return truncate(string(raw), 64)
}

// drainAndClose 与 xboard 包同名函数语义一致：吃完 body 让 keep-alive 复用。
func drainAndClose(rc io.ReadCloser) {
	_, _ = io.Copy(io.Discard, rc)
	_ = rc.Close()
}
