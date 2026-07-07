package web

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/xboard-bridge/xboard-xui-bridge/internal/store"
)

// deleteCleanupTimeout 是 handleDeleteBridge 在客户端断连后仍然完成
// reload + cleanup 所允许的最大时长。
//
// 选 30 秒：reload 内部用 reloadTimeout=15s 等旧引擎退出，加上 baseline
// 清理 + supervisor 切换状态的开销 < 5s；30s 留 2× 头空间足够。不设更长
// 是为了避免一个挂掉的 reload 让 handler goroutine 持续占用资源。
const deleteCleanupTimeout = 30 * time.Second

// bridgeResponse 是 GET /api/bridges 与单条返回共用的对外结构。
type bridgeResponse struct {
	Name           string `json:"name"`
	XboardNodeID   int    `json:"xboard_node_id"`
	XboardNodeType string `json:"xboard_node_type"`
	XuiPanel       string `json:"xui_panel"`
	XuiInboundID   int    `json:"xui_inbound_id"`
	Protocol       string `json:"protocol"`
	Flow           string `json:"flow,omitempty"`
	Enable         bool   `json:"enable"`
	CreatedAt      string `json:"created_at,omitempty"`
	UpdatedAt      string `json:"updated_at,omitempty"`
}

// bridgeRequest 是 POST / PUT 共用的请求体。
//
// 字段命名 snake_case：保持与 settings 一致；前端开发者一眼能联想到
// SQL 列名与 store.BridgeRow 字段。
type bridgeRequest struct {
	Name           string `json:"name"`
	XboardNodeID   int    `json:"xboard_node_id"`
	XboardNodeType string `json:"xboard_node_type"`
	XuiPanel       string `json:"xui_panel"`
	XuiInboundID   int    `json:"xui_inbound_id"`
	Protocol       string `json:"protocol"`
	Flow           string `json:"flow"`
	Enable         bool   `json:"enable"`
}

// handleListBridges 处理 GET /api/bridges。
func (s *Server) handleListBridges(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListBridges(r.Context())
	if err != nil {
		s.log.Error("ListBridges 失败", "err", err)
		s.writeError(w, http.StatusInternalServerError, errCodeInternal, "查询桥接列表失败")
		return
	}
	out := make([]bridgeResponse, 0, len(rows))
	for i := range rows {
		out = append(out, marshalBridge(&rows[i]))
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleCreateBridge 处理 POST /api/bridges。
//
// 行为：
//
//   1) 解析 body；
//   2) 调 store.CreateBridge（store 内部已做 name/protocol 非空校验 +
//      唯一约束映射）；
//   3) 调 reload；
//   4) 返回新建项。
//
// 注意：不在本 handler 做 protocol 取值合法性校验——store 层用 NOT NULL
// 仅保 name/protocol 非空，真正的"是否在 allowedProtocols 内"由
// LoadFromStore 后的 Validate 处理。如果用户提交 "qq-protocol" 这种
// 非法值，会先成功落库，然后 reload 触发 Validate 失败，handler 返回 5xx
// 让运维知道"已写入但引擎未切换"。可接受。
//
// 进一步的 v0.3 优化：在写库前直接调一次 cfg.Validate(stub *Root)
// 校验 protocol 合法性。M5 阶段先不做，避免维护两套校验逻辑。
func (s *Server) handleCreateBridge(w http.ResponseWriter, r *http.Request) {
	var req bridgeRequest
	if err := readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, errCodeBadRequest, "请求体格式错误")
		return
	}
	row := bridgeFromRequest(req)
	// 面板引用预检（fork 多面板扩展）：悬空引用若直接落库，reload 时
	// config.Validate 会失败并让 handler 返回 5xx——语义正确但对用户不友好。
	// 这里查一次真相源（xui_panels 表）提前给 400；不复制格式校验逻辑。
	if code, msg := s.checkPanelRef(r, req.XuiPanel); code != 0 {
		s.writeError(w, code, errCodeBadRequest, msg)
		return
	}
	if err := s.store.CreateBridge(r.Context(), row); err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyExists):
			s.writeError(w, http.StatusConflict, errCodeConflict, "桥接名已存在："+row.Name)
		default:
			// store 层已做 name/protocol 非空 trim 校验；其余错误（DB 故障 / SQLite BUSY）
			// 一律 500。
			s.log.Error("创建桥接失败",
				"event", "bridge_create_error",
				"name", row.Name,
				"err", err,
			)
			s.writeError(w, http.StatusInternalServerError, errCodeInternal, "创建桥接失败")
		}
		return
	}
	if err := s.reloadFromStore(r.Context()); err != nil {
		s.log.Error("创建桥接后引擎重载失败",
			"event", "bridge_create_reload_error",
			"name", row.Name,
			"err", err,
		)
		s.writeError(w, http.StatusInternalServerError, errCodeInternal, "桥接已保存但引擎重载失败，请检查日志")
		return
	}
	s.log.Info("桥接创建完成",
		"event", "bridge_created",
		"name", row.Name,
		"protocol", row.Protocol,
		"xboard_node_id", row.XboardNodeID,
		"xui_inbound_id", row.XuiInboundID,
		"enabled", row.Enable,
	)
	// 取一次最新行回传（带 created_at / updated_at）。
	saved, err := s.store.GetBridge(r.Context(), row.Name)
	if err != nil {
		// 极小概率：刚 create 完立刻 get 没拿到——不影响成功结果。
		s.writeJSON(w, http.StatusCreated, marshalBridge(&row))
		return
	}
	s.writeJSON(w, http.StatusCreated, marshalBridge(&saved))
}

// handleUpdateBridge 处理 PUT /api/bridges/{name}。
//
// 路径参数 {name} 必须与 body.name 一致——避免 URL 拼写漂移。
func (s *Server) handleUpdateBridge(w http.ResponseWriter, r *http.Request) {
	pathName := strings.TrimSpace(r.PathValue("name"))
	if pathName == "" {
		s.writeError(w, http.StatusBadRequest, errCodeBadRequest, "URL 缺少 name")
		return
	}
	var req bridgeRequest
	if err := readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, errCodeBadRequest, "请求体格式错误")
		return
	}
	if strings.TrimSpace(req.Name) != pathName {
		s.writeError(w, http.StatusBadRequest, errCodeBadRequest, "URL 中的 name 与 body 中的 name 不一致")
		return
	}
	row := bridgeFromRequest(req)
	// 面板引用预检：同 handleCreateBridge。
	if code, msg := s.checkPanelRef(r, req.XuiPanel); code != 0 {
		s.writeError(w, code, errCodeBadRequest, msg)
		return
	}
	if err := s.store.UpdateBridge(r.Context(), row); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.writeError(w, http.StatusNotFound, errCodeNotFound, "桥接不存在："+row.Name)
		default:
			s.log.Error("更新桥接失败",
				"event", "bridge_update_error",
				"name", row.Name,
				"err", err,
			)
			s.writeError(w, http.StatusInternalServerError, errCodeInternal, "更新桥接失败")
		}
		return
	}
	if err := s.reloadFromStore(r.Context()); err != nil {
		s.log.Error("更新桥接后引擎重载失败",
			"event", "bridge_update_reload_error",
			"name", row.Name,
			"err", err,
		)
		s.writeError(w, http.StatusInternalServerError, errCodeInternal, "桥接已保存但引擎重载失败，请检查日志")
		return
	}
	s.log.Info("桥接更新完成",
		"event", "bridge_updated",
		"name", row.Name,
		"protocol", row.Protocol,
		"enabled", row.Enable,
	)
	saved, err := s.store.GetBridge(r.Context(), row.Name)
	if err != nil {
		s.writeJSON(w, http.StatusOK, marshalBridge(&row))
		return
	}
	s.writeJSON(w, http.StatusOK, marshalBridge(&saved))
}

// handleDeleteBridge 处理 DELETE /api/bridges/{name}。
//
// store.DeleteBridge 是幂等的——本 handler 不区分"原本就不存在"与
// "成功删除"，统一返回 200 + 空 envelope。这是删除接口的常见行为：
// 客户端"删除我已删过的资源" 不应感知差异。
//
// 选 200 而非 204：本项目所有 API 走统一 envelope（apiResponse），204
// 不带 body 与该约定冲突；若某些前端代码无差别 JSON.parse 响应会因 204
// 空 body 报错。返回 `{}` envelope 让前端代码路径完全统一。
//
// 留给 v0.3 的清理项：当前 M5 阶段 traffic_baseline 中以该 bridge_name
// 为主键的所有行不会被清理——store 没有"按 bridge_name 批量删除"的接口；
// user_sync 仅在用户被禁/删时单条删 baseline。删除桥接后这部分数据成为
// 无主行，占用极少（< 1KB / user）。v0.3 计划新增
// store.DeleteBaselinesByBridge 一次性清理。
// handleDeleteBridge 实现"删 bridge → 停旧 worker → 清 baseline"的正确顺序。
//
// 顺序约束（Codex 审查指出的并发竞态）：
//
//	a) 先删 bridges 行（store 写入立即可见）；
//	b) 调 reloadFromStore：supervisor.Reload 让旧 sync worker 停掉
//	   （新 cfg 已不含该 bridge → 新 engine 不会再装配 worker）；
//	c) 此时**没有任何 goroutine**会再访问该 bridge_name 的 baseline——
//	   调 DeleteBaselinesByBridge 清理可放心进行。
//
// 旧实现（v0.8.4 之前）把 a 与 c 合并到一个事务里，结果：旧 sync worker
// 在 reload 完成前仍会调 EnsureBaseline / RecordSeen / ApplyReportSuccess
// 把刚清掉的 baseline 重新写回——清理失效。新流程让 c 必然发生在旧 worker
// 退出之后，杜绝并发写覆盖。
//
// 容错策略：a 成功后 b 失败时，bridges 表已无该行 + 旧 engine 仍在跑该
// bridge → 旧 worker 下次访问 baseline 时不会发现 user_sync 的 wanted 状态
// （Xboard 仍返回用户）但 3x-ui 端 inbound 还在——它会继续维护 baseline，
// 直到下次重启 / 下次 reload 成功。Web 返回 5xx 让运维介入；c 不执行
// （baseline 残留但不影响功能）。
//
// c 失败但 a + b 成功时：bridge 已被彻底移除、baseline 残留。残留量按典型
// 几千用户 × 数十字节 ≈ < 1 MB，对 SQLite 无感；下次重启进程不会自动清理
// （没有"定时清理孤儿 baseline"机制），运维可手工 sqlite3 删除或下次该
// bridge 名被重用时手动清。仅打 WARN 日志，response 仍返回 200——避免
// 让运维以为"删除失败要重试"。
//
// 客户端断连防护（v0.8.4 二轮 Codex 审查指出）：步骤 a 写入持久层后，若
// 仍使用 r.Context() 做 b/c，客户端在 a 之后断连会让 r.Context() 立即取消，
// b/c 被中断——bridges 行已删但 engine 仍跑旧 worker + baseline 残留。
// 改为 a 仍用 r.Context()（用户主动断连是合理的"取消请求"信号，store 写
// 失败不会留下副作用），b/c 用独立 bounded ctx（deleteCleanupTimeout）：
// 一旦写库成功，后续清理就必须跑完，不再受请求生命周期约束。30s 上限避
// 免 handler goroutine 因 reload 卡死被永久占用。
func (s *Server) handleDeleteBridge(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		s.writeError(w, http.StatusBadRequest, errCodeBadRequest, "URL 缺少 name")
		return
	}
	// 步骤 1：删 bridges 行（不动 baseline）。仍走 r.Context()——客户端
	// 此时主动断连是合理的"取消请求"信号，store.DeleteBridge 是单条
	// DELETE 语句，要么成功要么失败，无中间状态副作用。
	if err := s.store.DeleteBridge(r.Context(), name); err != nil {
		s.log.Error("删除桥接失败",
			"event", "bridge_delete_error",
			"name", name,
			"err", err,
		)
		s.writeError(w, http.StatusInternalServerError, errCodeInternal, "删除桥接失败")
		return
	}

	// 步骤 2/3 切到独立 bounded ctx：客户端断连不再能中断后续清理；详见
	// 函数注释"客户端断连防护"段落。仍记录用户端原 ctx 是否已取消供日志
	// 观察——但不参与控制流。
	cleanupCtx, cancel := context.WithTimeout(context.Background(), deleteCleanupTimeout)
	defer cancel()
	if rerr := r.Context().Err(); rerr != nil {
		s.log.Info("桥接删除：客户端在写库后断连，cleanup 阶段切换独立 ctx 继续",
			"name", name,
			"req_ctx_err", rerr,
		)
	}

	// 步骤 2：reload 让旧 sync worker 退出。
	if err := s.reloadFromStore(cleanupCtx); err != nil {
		s.log.Error("删除桥接后引擎重载失败",
			"event", "bridge_delete_reload_error",
			"name", name,
			"err", err,
		)
		// 客户端可能已断开；写 5xx 是 best-effort——若写失败 net/http 内部
		// 会忽略，不影响 cleanup 路径。
		s.writeError(w, http.StatusInternalServerError, errCodeInternal, "桥接已删除但引擎重载失败，请检查日志")
		return
	}
	// 步骤 3：清 baseline。此时已无 worker 访问，安全。
	deletedBaselines, err := s.store.DeleteBaselinesByBridge(cleanupCtx, name)
	if err != nil {
		// baseline 清理失败不影响 bridge 删除成功；记 WARN 让运维知情。
		s.log.Warn("桥接删除：traffic_baseline 清理失败（不影响业务，下次重启后可手工清理）",
			"event", "bridge_delete_baseline_cleanup_failed",
			"name", name,
			"err", err,
		)
	} else if deletedBaselines > 0 {
		s.log.Info("桥接删除：连带清理 traffic_baseline",
			"event", "bridge_deleted",
			"name", name,
			"deleted_baselines", deletedBaselines,
		)
	} else {
		s.log.Info("桥接删除完成",
			"event", "bridge_deleted",
			"name", name,
		)
	}
	s.writeJSON(w, http.StatusOK, struct{}{})
}

// checkPanelRef 校验桥接引用的面板名非空且存在于 xui_panels 表。
//
// 返回值：(0, "") 表示通过；否则返回应写给客户端的 HTTP 状态码与消息。
// 查询故障按 500 处理——无法确认引用有效性时宁可拒绝写入，也不让悬空
// 引用落库触发后续 reload 失败。
func (s *Server) checkPanelRef(r *http.Request, panelName string) (int, string) {
	name := strings.TrimSpace(panelName)
	if name == "" {
		return http.StatusBadRequest, "xui_panel 不可为空（请先在「面板」页创建 3x-ui 面板）"
	}
	if _, err := s.store.GetXuiPanel(r.Context(), name); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return http.StatusBadRequest, "xui_panel 引用的面板不存在：" + name
		}
		s.log.Error("校验面板引用失败", "panel", name, "err", err)
		return http.StatusInternalServerError, "校验面板引用失败"
	}
	return 0, ""
}

// bridgeFromRequest 把请求 DTO 转换为 store.BridgeRow。
//
// 不做 trim / lower：这些规范化在 LoadFromStore 后的 Validate 中执行。
// 直接保留原值落库，让运维下次 GET 看到自己输入了什么——便于排错"为什么
// 我的字段被悄悄改了"。
func bridgeFromRequest(req bridgeRequest) store.BridgeRow {
	return store.BridgeRow{
		Name:           req.Name,
		XboardNodeID:   req.XboardNodeID,
		XboardNodeType: req.XboardNodeType,
		XuiPanel:       req.XuiPanel,
		XuiInboundID:   req.XuiInboundID,
		Protocol:       req.Protocol,
		Flow:           req.Flow,
		Enable:         req.Enable,
	}
}

// marshalBridge 把 store.BridgeRow 投影到 bridgeResponse。
//
// 时间字段用 RFC3339；零值时间不输出（让前端判断"暂未发生"）。
func marshalBridge(row *store.BridgeRow) bridgeResponse {
	resp := bridgeResponse{
		Name:           row.Name,
		XboardNodeID:   row.XboardNodeID,
		XboardNodeType: row.XboardNodeType,
		XuiPanel:       row.XuiPanel,
		XuiInboundID:   row.XuiInboundID,
		Protocol:       row.Protocol,
		Flow:           row.Flow,
		Enable:         row.Enable,
	}
	if !row.CreatedAt.IsZero() {
		resp.CreatedAt = row.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !row.UpdatedAt.IsZero() {
		resp.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return resp
}
