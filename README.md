<div align="center">

# xboard-xui-bridge（多面板 fork）

**[Xboard](https://github.com/cedar2025/Xboard) 与 [3x-ui](https://github.com/MHSanaei/3x-ui) 的非侵入式中间件**

不改源码、不在节点机部署 XrayR / V2bX，只需一个二进制让两套面板无缝对接。

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/badge/Go-1.25%2B-00ADD8?logo=go)](go.mod)
[![Vue Version](https://img.shields.io/badge/Vue-3-4FC08D?logo=vue.js)](web/package.json)

</div>

> [!NOTE]
> 本仓库是 [ZeroStarlet/xboard-xui-bridge](https://github.com/ZeroStarlet/xboard-xui-bridge) 的 fork，
> 在上游基础上**新增了多面板能力**：一个中间件实例可同时对接**多台 3x-ui 面板**
> （即多台节点 VPS），配合 Web 面板集中管理。安装脚本与 Release 均指向本 fork
> （`A-pursuer/x-bridge`）。

> [!IMPORTANT]
> 本项目仅用于个人使用和通信，请勿将其用于非法目的。

---

## 与上游的核心差异

| 维度 | 上游 | 本 fork |
| --- | --- | --- |
| 3x-ui 对接 | 单实例 = 单面板（settings 里一份 `xui.*`） | 单实例 = **N 个面板**（独立 `xui_panels` 表） |
| 部署形态 | 每台节点各跑一个中间件 | **中心化**：一个实例集中管多台节点的 3x-ui |
| 面板配置 | 设置页里填 3x-ui 地址/令牌 | 独立「3x-ui 面板」页，支持**测试连接** |
| 桥接 | 每条桥接隐含唯一面板 | 每条桥接**显式选择所属面板**（`xui_panel`） |
| 可观测性 | 同步结果只在日志 | 桥接卡片显示**最近同步**状态灯 |

典型架构：**1 个 Xboard × N 台 3x-ui（每台若干 inbound）× 任意条桥接**。
中间件推荐与 Xboard 部署在同机，对 Xboard 走 localhost，对各节点 3x-ui 走
Tailscale 内网。

从上游或旧版升级时，原来的单面板 `xui.*` 配置会在首次启动时**自动迁移**为
一个名为 `default` 的面板，存量桥接自动指向它，升级无感。

---

## 快速入门

### 一键安装（Linux）

```bash
bash <(curl -fsSL https://raw.githubusercontent.com/A-pursuer/x-bridge/main/install.sh)
```

未安装则安装最新 Release，已安装则进入管理菜单。二进制经 SHA256 校验后落地。

### 准备工作

| 系统 | 准备项 |
| --- | --- |
| Xboard | 拿到 `通讯密钥`；为每个节点分配数字 ID。 |
| 3x-ui | **必须 v3.0.0+**。每台面板「面板设置 → API 令牌」生成 48 字符 Token；预创建 inbound 拿到 ID。 |
| Xboard 队列 | **必须**运行 `php artisan queue:work` 或 Horizon——否则中间件 push 永远成功但用户 u/d 永远 0。 |
| 内网（多节点） | 各节点 3x-ui 建议监听 Tailscale IP（而非公网），中间件用 `http://100.x.x.x:端口` 对接。 |

### `xui-bridge` 常用命令

```bash
xui-bridge install              # 安装 / 升级到本 fork 最新版
xui-bridge start | stop | restart
xui-bridge status               # systemctl 状态
xui-bridge log | follow         # 日志 / 实时跟踪
xui-bridge reset-password       # 忘密码兜底
xui-bridge change-listen-addr   # 改监听地址
xui-bridge uninstall | purge    # 卸载（保留数据 / 含数据）
xui-bridge menu                 # 交互菜单（默认）
```

---

## 使用流程

安装完成后浏览器打开 `http://<监听地址>:8787`（默认 `admin` + 初始密码见安装输出 /
`data/initial_password.txt`），首次登录后立即改密并删除该文件。

1. **添加面板**：进「3x-ui 面板」页 → 新增 → 填 `API 服务地址`（含端口，如
   `http://100.64.0.1:2053`）、`面板路径前缀`（3x-ui 的 webBasePath，无则留空）、
   `API 令牌` → 点**「测试连接」**确认连通+鉴权通过 → 保存。
   - 每台节点 VPS 的 3x-ui 建一个面板。
2. **建桥接**：进「桥接管理」页 → 新增 → 选**所属面板** + 填 Xboard 节点 ID、
   3x-ui inbound ID、协议（vless / hysteria2 等）→ 保存。
3. **看状态**：桥接卡片底部「最近同步」灯——绿=同步正常，红=失败（悬停看错误），
   灰=等待首次同步。Xboard 后台节点圆点转黄/绿即对接成功。

> [!TIP]
> 常见坑：面板地址**漏写端口**会打到 80/443 上的其他服务、收到伪装页导致同步失败。
> 保存前用「测试连接」即可提前发现。

---

## 中心化部署（多节点）建议

- **中间件**：与 Xboard 同机；对 Xboard 用 `http://127.0.0.1:...`，对各节点 3x-ui 用
  Tailscale IP。
- **各节点 3x-ui**：面板监听改为自己的 Tailscale IP（公网不暴露面板端口），
  生成各自的 API 令牌。
- **Web 面板（8787）**：绑定中间件所在机的 Tailscale IP，公网不放行 8787；
  仅 tailnet 内设备可访问（或 `ssh -L` 端口转发）。
- **单点意识**：中心实例挂 → 全部节点停止同步（存量用户不断线、流量差量模型
  保证恢复后不重复计费，但新用户下发暂停）。建议用 Nezha 等监控 bridge 服务存活。
- **密钥集中**：`bridge.db`（默认 0600）集中存放全部 3x-ui 令牌 + Xboard 通讯密钥，
  备份拷出机器前请加密。

---

## 手工编译

```bash
make build-all           # 端到端：构建 Vue 前端 → 嵌入 Go 二进制
make build               # 仅 Go（要求 internal/web/dist 已最新）
make build-linux         # 交叉编译 Linux x86_64
make test                # 跑单元测试
```

前端用 `npm ci`（按 `web/package-lock.json` 锁定版本，可复现）。二进制输出到 `dist/`。

### 本地开发

```bash
# 终端 1：Vite dev server
cd web && npm run dev

# 终端 2：Go 后端
make build && ./dist/xboard-xui-bridge run --db ./data/bridge.db
```

Vite 已配置代理 `/api → 127.0.0.1:8787`。

---

## 质量保障

- **CI**（`.github/workflows/ci.yaml`）：每次 push / PR 跑 `go vet` + `go test`。
- **单元测试**覆盖核心逻辑：旧库单面板→多面板迁移、config 校验（面板引用/重名/
  协议映射）、面板 CRUD、同步状态注册表并发安全。
- **发版**（`.github/workflows/release.yaml`）：推 `v*` 标签 → CI 用 `npm ci` 重建
  前端 + 编译三架构 + SHA256SUMS.txt → GitHub Release。

---

## 致谢

- [Xboard](https://github.com/cedar2025/Xboard) — Laravel 高性能销售面板
- [3x-ui](https://github.com/MHSanaei/3x-ui) — Xray 多协议网页面板
- [ZeroStarlet/xboard-xui-bridge](https://github.com/ZeroStarlet/xboard-xui-bridge) — 本 fork 的上游

## 协议

本项目以 [MIT License](LICENSE) 发布。
