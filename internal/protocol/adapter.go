package protocol

import (
	"fmt"

	"github.com/xboard-bridge/xboard-xui-bridge/internal/config"
	"github.com/xboard-bridge/xboard-xui-bridge/internal/xboard"
	"github.com/xboard-bridge/xboard-xui-bridge/internal/xui"
)

// Adapter 描述某种协议的"Xboard 用户 ↔ 3x-ui client"双向规则。
//
// 每个协议实现一个 Adapter；同步引擎根据 Bridge.Protocol 通过 For() 取得对应实例。
//
// 接口故意保持极小——只要双方互相理解 ClientSettings，加协议无需改动接口。
//
// 历史：v0.8.2 之前接口还含 KeyOf（为旧端点
// POST /panel/api/inbounds/updateClient/:clientKey 提取主键），但 3x-ui v3.3+
// 把所有 client 增删改端点搬到 /panel/api/clients/* 且 URL 主键统一为 email，
// clientKey 概念消失——故 v0.8.3 起 Adapter 不再需要 KeyOf。
type Adapter interface {
	// Protocol 返回适配器所对应的协议名（小写、与 config.ProtocolXxx 一致）。
	Protocol() string

	// BuildClient 把 Xboard 用户转换为可写入 3x-ui inbound.settings.clients[i] 的对象。
	BuildClient(user xboard.User, bridge config.Bridge) xui.ClientSettings
}

// For 按协议名取适配器；未实现的协议返回非 nil error。
//
// 关键设计：以函数返回新实例而不是单例，给将来"按 Bridge 注入差异化默认值"留口；
// 现阶段实现都是无状态的，每次构造也几乎零开销（无堆分配，仅返回小结构体值）。
func For(protocol string) (Adapter, error) {
	switch protocol {
	case config.ProtocolVLESS:
		return vlessAdapter{}, nil
	case config.ProtocolVMess:
		return vmessAdapter{}, nil
	case config.ProtocolTrojan:
		return trojanAdapter{}, nil
	case config.ProtocolShadowsocks:
		return shadowsocksAdapter{}, nil
	case config.ProtocolHysteria:
		return hysteriaV1Adapter{}, nil
	case config.ProtocolHysteria2:
		return hysteriaV2Adapter{}, nil
	default:
		return nil, fmt.Errorf("protocol: 暂不支持 %q 协议", protocol)
	}
}

// totalGBFromTransfer 把 Xboard 用户的"剩余流量"概念映射为 3x-ui 的 totalGB。
//
// Xboard 在 V2 user 端点中并未直接返回 transfer_enable 字段（仅 id/uuid/speed_limit/device_limit），
// 这是因为流量控制的"真相源"在 Xboard，而 3x-ui 端的限额仅作冗余兜底。
// 中间件不在 3x-ui 写入 totalGB，让 3x-ui 端保持"无限"，所有限流靠 Xboard
// 周期下发用户列表（被禁用的用户会从 user 列表消失，diff 时自然被删除）。
//
// 因此本函数固定返回 0（即 3x-ui 端"无限"语义），仅留作单点修改入口；
// 未来若 Xboard 暴露 transfer_enable 字段，可以在此处转换为字节数。
func totalGBFromTransfer(_ xboard.User) int64 { return 0 }

// expiryFromUser 同理：用户过期由 Xboard 控制，3x-ui 端不设过期。
// 0 表示永不过期（3x-ui 规约：expiryTime <= 0 即视为不过期）。
func expiryFromUser(_ xboard.User) int64 { return 0 }
