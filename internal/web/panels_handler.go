package web

import (
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/xboard-bridge/xboard-xui-bridge/internal/store"
)

// panels_handler.go 实现 /api/xui-panels 的 CRUD（fork 多面板扩展）。
//
// 设计与 bridges_handler.go 保持完全一致的风格：
//
//   - store 层只做非空 / 唯一约束等结构校验；api_host / api_token 的格式
//     校验统一由 reload 路径上的 LoadFromStore + config.Validate 处理，
//     不维护两套校验逻辑；
//   - 每次写操作成功后调 reloadFromStore：面板凭据变更即时体现为
//     supervisor 重建对应 xui.Client（与旧版"改 xui.* settings 触发
//     reload"的语义等价）；
//   - 删除面板前校验无 bridge 引用——引用完整性由业务层显式编排（schema
//     无 FK 约束，与 sessions.user_id 的处理方式一致）。

// panelResponse 是 GET /api/xui-panels 与单条返回共用的对外结构。
//
// api_token 明文回传：访问者已通过 admin 鉴权，与 settings 的
// xboard.token 回传策略一致；前端用 type=password 隐藏显示。
type panelResponse struct {
	Name          string `json:"name"`
	APIHost       string `json:"api_host"`
	BasePath      string `json:"base_path"`
	APIToken      string `json:"api_token"`
	TimeoutSec    int    `json:"timeout_sec"`
	SkipTLSVerify bool   `json:"skip_tls_verify"`
	// BridgeCount 是引用本面板的桥接数；列表页据此提示"删除前需先解除引用"。
	BridgeCount int    `json:"bridge_count"`
	CreatedAt   string `json:"created_at,omitempty"`
	UpdatedAt   string `json:"updated_at,omitempty"`
}

// panelRequest 是 POST / PUT 共用的请求体。
type panelRequest struct {
	Name          string `json:"name"`
	APIHost       string `json:"api_host"`
	BasePath      string `json:"base_path"`
	APIToken      string `json:"api_token"`
	TimeoutSec    int    `json:"timeout_sec"`
	SkipTLSVerify bool   `json:"skip_tls_verify"`
}

// handleListPanels 处理 GET /api/xui-panels。
func (s *Server) handleListPanels(w http.ResponseWriter, r *http.Request) {
	rows, err := s.store.ListXuiPanels(r.Context())
	if err != nil {
		s.log.Error("ListXuiPanels 失败", "err", err)
		s.writeError(w, http.StatusInternalServerError, errCodeInternal, "查询面板列表失败")
		return
	}
	out := make([]panelResponse, 0, len(rows))
	for i := range rows {
		resp := marshalPanel(&rows[i])
		// 引用计数失败不阻断列表——它只是提示性字段，出错时保持 0 并打 WARN。
		if n, cerr := s.store.CountBridgesByPanel(r.Context(), rows[i].Name); cerr == nil {
			resp.BridgeCount = n
		} else {
			s.log.Warn("统计面板引用数失败（非致命）", "panel", rows[i].Name, "err", cerr)
		}
		out = append(out, resp)
	}
	s.writeJSON(w, http.StatusOK, out)
}

// handleCreatePanel 处理 POST /api/xui-panels。
func (s *Server) handleCreatePanel(w http.ResponseWriter, r *http.Request) {
	var req panelRequest
	if err := readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, errCodeBadRequest, "请求体格式错误")
		return
	}
	row := panelFromRequest(req)
	if strings.TrimSpace(row.Name) == "" {
		s.writeError(w, http.StatusBadRequest, errCodeBadRequest, "面板名不可为空")
		return
	}
	if err := s.store.CreateXuiPanel(r.Context(), row); err != nil {
		switch {
		case errors.Is(err, store.ErrAlreadyExists):
			s.writeError(w, http.StatusConflict, errCodeConflict, "面板名已存在："+row.Name)
		default:
			s.log.Error("创建面板失败",
				"event", "panel_create_error",
				"name", row.Name,
				"err", err,
			)
			s.writeError(w, http.StatusInternalServerError, errCodeInternal, "创建面板失败")
		}
		return
	}
	if err := s.reloadFromStore(r.Context()); err != nil {
		s.log.Error("创建面板后引擎重载失败",
			"event", "panel_create_reload_error",
			"name", row.Name,
			"err", err,
		)
		s.writeError(w, http.StatusInternalServerError, errCodeInternal, "面板已保存但引擎重载失败，请检查日志")
		return
	}
	s.log.Info("面板创建完成",
		"event", "panel_created",
		"name", row.Name,
		"api_host", row.APIHost,
	)
	saved, err := s.store.GetXuiPanel(r.Context(), row.Name)
	if err != nil {
		s.writeJSON(w, http.StatusCreated, marshalPanel(&row))
		return
	}
	s.writeJSON(w, http.StatusCreated, marshalPanel(&saved))
}

// handleUpdatePanel 处理 PUT /api/xui-panels/{name}。
//
// 与 bridges 一致：路径参数 {name} 必须与 body.name 一致，不支持改名——
// 面板名被 bridges.xui_panel 按值引用，改名需要级联更新引用，运维价值低
// 而实现复杂度高；需要"改名"时按"新建 + 迁移桥接 + 删旧"操作。
func (s *Server) handleUpdatePanel(w http.ResponseWriter, r *http.Request) {
	pathName := strings.TrimSpace(r.PathValue("name"))
	if pathName == "" {
		s.writeError(w, http.StatusBadRequest, errCodeBadRequest, "URL 缺少 name")
		return
	}
	var req panelRequest
	if err := readJSON(r, &req); err != nil {
		s.writeError(w, http.StatusBadRequest, errCodeBadRequest, "请求体格式错误")
		return
	}
	if strings.TrimSpace(req.Name) != pathName {
		s.writeError(w, http.StatusBadRequest, errCodeBadRequest, "URL 中的 name 与 body 中的 name 不一致（面板不支持改名）")
		return
	}
	row := panelFromRequest(req)
	if err := s.store.UpdateXuiPanel(r.Context(), row); err != nil {
		switch {
		case errors.Is(err, store.ErrNotFound):
			s.writeError(w, http.StatusNotFound, errCodeNotFound, "面板不存在："+row.Name)
		default:
			s.log.Error("更新面板失败",
				"event", "panel_update_error",
				"name", row.Name,
				"err", err,
			)
			s.writeError(w, http.StatusInternalServerError, errCodeInternal, "更新面板失败")
		}
		return
	}
	if err := s.reloadFromStore(r.Context()); err != nil {
		s.log.Error("更新面板后引擎重载失败",
			"event", "panel_update_reload_error",
			"name", row.Name,
			"err", err,
		)
		s.writeError(w, http.StatusInternalServerError, errCodeInternal, "面板已保存但引擎重载失败，请检查日志")
		return
	}
	s.log.Info("面板更新完成",
		"event", "panel_updated",
		"name", row.Name,
		"api_host", row.APIHost,
	)
	saved, err := s.store.GetXuiPanel(r.Context(), row.Name)
	if err != nil {
		s.writeJSON(w, http.StatusOK, marshalPanel(&row))
		return
	}
	s.writeJSON(w, http.StatusOK, marshalPanel(&saved))
}

// handleDeletePanel 处理 DELETE /api/xui-panels/{name}。
//
// 引用校验：仍被 bridge 引用的面板不可删——否则下次 LoadFromStore 的
// Validate 会因悬空引用整体失败，运维被迫用 sqlite3 手工修库。409 提示
// 运维先删除或迁移相关桥接。
//
// 校验与删除之间存在并发窗口（另一会话同时创建引用该面板的 bridge）——
// 概率极低且后果可控（reload 失败报错，配置仍可在 Web 修复），不为此
// 引入跨表事务。
func (s *Server) handleDeletePanel(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.PathValue("name"))
	if name == "" {
		s.writeError(w, http.StatusBadRequest, errCodeBadRequest, "URL 缺少 name")
		return
	}
	n, err := s.store.CountBridgesByPanel(r.Context(), name)
	if err != nil {
		s.log.Error("统计面板引用数失败",
			"event", "panel_delete_error",
			"name", name,
			"err", err,
		)
		s.writeError(w, http.StatusInternalServerError, errCodeInternal, "删除面板失败")
		return
	}
	if n > 0 {
		s.writeError(w, http.StatusConflict, errCodeConflict,
			"该面板仍被桥接引用，无法删除；请先删除或迁移相关桥接")
		return
	}
	if err := s.store.DeleteXuiPanel(r.Context(), name); err != nil {
		s.log.Error("删除面板失败",
			"event", "panel_delete_error",
			"name", name,
			"err", err,
		)
		s.writeError(w, http.StatusInternalServerError, errCodeInternal, "删除面板失败")
		return
	}
	if err := s.reloadFromStore(r.Context()); err != nil {
		s.log.Error("删除面板后引擎重载失败",
			"event", "panel_delete_reload_error",
			"name", name,
			"err", err,
		)
		s.writeError(w, http.StatusInternalServerError, errCodeInternal, "面板已删除但引擎重载失败，请检查日志")
		return
	}
	s.log.Info("面板删除完成",
		"event", "panel_deleted",
		"name", name,
	)
	s.writeJSON(w, http.StatusOK, struct{}{})
}

// panelFromRequest 把请求 DTO 转换为 store.XuiPanelRow。
//
// 与 bridgeFromRequest 同策略：不做 trim / 格式校验，保留原值落库，由
// reload 路径的 config.Validate 统一把关。
func panelFromRequest(req panelRequest) store.XuiPanelRow {
	return store.XuiPanelRow{
		Name:          req.Name,
		APIHost:       req.APIHost,
		BasePath:      req.BasePath,
		APIToken:      req.APIToken,
		TimeoutSec:    req.TimeoutSec,
		SkipTLSVerify: req.SkipTLSVerify,
	}
}

// marshalPanel 把 store.XuiPanelRow 投影到 panelResponse。
func marshalPanel(row *store.XuiPanelRow) panelResponse {
	resp := panelResponse{
		Name:          row.Name,
		APIHost:       row.APIHost,
		BasePath:      row.BasePath,
		APIToken:      row.APIToken,
		TimeoutSec:    row.TimeoutSec,
		SkipTLSVerify: row.SkipTLSVerify,
	}
	if !row.CreatedAt.IsZero() {
		resp.CreatedAt = row.CreatedAt.UTC().Format(time.RFC3339)
	}
	if !row.UpdatedAt.IsZero() {
		resp.UpdatedAt = row.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return resp
}
