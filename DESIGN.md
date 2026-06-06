# Linux 全局快捷键工具 — 技术方案

## 一、架构概览

```
┌─────────────────────────────────────────────────────────┐
│                  gohotkey daemon (root)                  │
├─────────────────────────────────────────────────────────┤
│                                                         │
│  ┌───────────────┐    ┌───────────────────────────┐    │
│  │ Config Loader │───▶│ Hotkey Registry (map)     │    │
│  │ (YAML + HUP)  │    │ modifiers+key → Command   │    │
│  └───────────────┘    └─────────────┬─────────────┘    │
│                                     │ lookup            │
│  ┌───────────────┐    ┌─────────────▼─────────────┐    │
│  │ Device Watcher│───▶│ Event Dispatcher          │    │
│  │ (udev/inotify)│    │ - modifier state bitmap   │    │
│  └───────────────┘    │ - key match logic         │    │
│                       └─────────────┬─────────────┘    │
│  ┌───────────────┐                  │ matched          │
│  │ evdev Reader  │                  ▼                  │
│  │ (per device)  │    ┌───────────────────────────┐    │
│  │ goroutine     │───▶│ Command Executor          │    │
│  └───────────────┘    │ - 降权执行 (setuid)        │    │
│                       │ - 环境变量注入             │    │
│                       │ - 异步 + 超时控制          │    │
│                       └───────────────────────────┘    │
└─────────────────────────────────────────────────────────┘
```

## 二、核心模块设计

### 2.1 evdev 事件读取

**原理：** 直接读取 `/dev/input/eventN` 设备文件，解析 Linux `input_event` 结构体。

**数据结构（内核定义）：**

```go
// 对应 linux/input.h 中的 struct input_event
type InputEvent struct {
    Time  syscall.Timeval // 16 bytes
    Type  uint16          // 事件类型 (EV_KEY = 0x01)
    Code  uint16          // 键码 (KEY_A = 30, KEY_LEFTCTRL = 29, ...)
    Value int32           // 0=释放, 1=按下, 2=重复
}
// 总大小：24 bytes（amd64）
```

**读取方式：**

```go
func readEvents(devicePath string, ch chan<- InputEvent) {
    f, _ := os.Open(devicePath)
    defer f.Close()
    buf := make([]byte, 24)
    for {
        _, err := io.ReadFull(f, buf)
        if err != nil { return }
        ev := parseInputEvent(buf)
        if ev.Type == EV_KEY {
            ch <- ev
        }
    }
}
```

### 2.2 键盘设备识别

**问题：** `/dev/input/` 下有键盘、鼠标、触摸板、电源按钮等几十个设备，需要筛选出键盘。

**判定规则：** 设备同时具备以下能力位：

- `EV_KEY`（能产生按键事件）
- 包含 `KEY_A` ~ `KEY_Z` 中的至少一部分（排除仅有几个特殊键的设备，如电源按钮）

**实现：** 通过 `ioctl(fd, EVIOCGBIT(EV_KEY, ...), bitmap)` 拿到能力位图，做位检查。

**热插拔识别：** 通过 `inotify` 监听 `/dev/input/` 目录的 `IN_CREATE` 事件，新设备出现时启动新的读取 goroutine。

### 2.3 组合键状态机

**思路：** 维护一个 modifier 位图，按键事件到来时更新位图，匹配到目标键时检查 modifier 状态。

```go
type ModifierMask uint8

const (
    ModCtrl  ModifierMask = 1 << 0
    ModAlt   ModifierMask = 1 << 1
    ModShift ModifierMask = 1 << 2
    ModSuper ModifierMask = 1 << 3
)

type Matcher struct {
    state    ModifierMask           // 当前 modifier 按下状态
    bindings map[BindingKey]*Action // 配置的快捷键
}

type BindingKey struct {
    Mods ModifierMask
    Key  uint16
}

func (m *Matcher) OnEvent(ev InputEvent) *Action {
    switch ev.Code {
    case KEY_LEFTCTRL, KEY_RIGHTCTRL:
        m.updateMod(ModCtrl, ev.Value)
        return nil
    case KEY_LEFTALT, KEY_RIGHTALT:
        m.updateMod(ModAlt, ev.Value)
        return nil
    // ... 其他 modifier
    }
    if ev.Value == 1 { // 仅在按下时触发
        key := BindingKey{Mods: m.state, Key: ev.Code}
        return m.bindings[key]
    }
    return nil
}
```

**注意事项：**

- **多键盘的 modifier 同步问题：** 每个键盘维护独立的 modifier 状态，避免 A 键盘按 Ctrl + B 键盘按 T 触发组合键
- **按键释放丢失：** 程序启动时如果 Ctrl 已经被按下，无法感知。需在每个键盘 fd 上调用 `EVIOCGKEY` 读取初始按键状态

### 2.4 命令执行与降权

**问题：** 守护进程以 root 运行，但 `alacritty`、`nautilus` 等 GUI 程序需要以普通用户身份启动，且需要正确的 `DISPLAY` / `XDG_RUNTIME_DIR` 环境变量。

**降权方案：**

```go
func runCommand(action *Action) {
    cmd := exec.Command("sh", "-c", action.Command)

    // 降权到目标用户
    uid, gid := lookupUser(action.User)
    cmd.SysProcAttr = &syscall.SysProcAttr{
        Credential: &syscall.Credential{Uid: uid, Gid: gid},
        Setsid:     true, // 脱离 daemon 的进程组
    }

    // 注入用户环境变量
    cmd.Env = buildUserEnv(action.User, action.Env)
    cmd.Dir = action.WorkDir

    if err := cmd.Start(); err != nil {
        log.Errorf("命令启动失败：%v", err)
        return
    }
    go cmd.Wait() // 异步回收，避免僵尸进程
}

func buildUserEnv(username string, extra map[string]string) []string {
    u, _ := user.Lookup(username)
    env := []string{
        "HOME=" + u.HomeDir,
        "USER=" + username,
        "PATH=/usr/local/bin:/usr/bin:/bin",
        "DISPLAY=:0",                                      // X11
        "WAYLAND_DISPLAY=wayland-0",                       // Wayland
        "XDG_RUNTIME_DIR=/run/user/" + u.Uid,              // 关键，否则 GUI 程序起不来
        "DBUS_SESSION_BUS_ADDRESS=unix:path=/run/user/" + u.Uid + "/bus",
    }
    for k, v := range extra {
        env = append(env, k+"="+v)
    }
    return env
}
```

### 2.5 配置文件设计

**路径：** `/etc/gohotkey/config.yaml`

**示例：**

```yaml
# 全局默认配置
defaults:
  user: tony            # 默认以该用户身份执行命令
  env:
    DISPLAY: ":0"
    WAYLAND_DISPLAY: "wayland-0"

# 快捷键绑定
hotkeys:
  - name: 打开终端
    keys: ctrl+alt+t
    command: /usr/bin/alacritty

  - name: 打开文件管理器
    keys: super+e
    command: /usr/bin/nautilus

  - name: 锁屏
    keys: super+l
    command: loginctl lock-session
    user: root          # 覆盖默认用户

  - name: 截图
    keys: shift+printscreen
    command: /usr/bin/flameshot gui
    workdir: /home/tony/Pictures
```

**键名映射表（部分）：**

| 配置键名 | evdev 常量 | 键码 |
|---------|-----------|------|
| `ctrl` | `KEY_LEFTCTRL` / `KEY_RIGHTCTRL` | 29 / 97 |
| `alt` | `KEY_LEFTALT` / `KEY_RIGHTALT` | 56 / 100 |
| `shift` | `KEY_LEFTSHIFT` / `KEY_RIGHTSHIFT` | 42 / 54 |
| `super` | `KEY_LEFTMETA` / `KEY_RIGHTMETA` | 125 / 126 |
| `a` ~ `z` | `KEY_A` ~ `KEY_Z` | 30 ~ 44 |
| `f1` ~ `f12` | `KEY_F1` ~ `KEY_F12` | 59 ~ 88 |
| `printscreen` | `KEY_SYSRQ` | 99 |

完整映射会从 `/usr/include/linux/input-event-codes.h` 生成。

## 三、项目结构

```
autohotkey/
├── REQUIREMENTS.md          # 需求文档
├── DESIGN.md                # 技术方案（本文档）
├── README.md                # 用户文档
├── go.mod
├── go.sum
├── cmd/
│   └── gohotkey/
│       └── main.go          # 入口，CLI 参数解析
├── internal/
│   ├── evdev/
│   │   ├── device.go        # 设备发现、能力检测
│   │   ├── event.go         # input_event 解析
│   │   └── keycode.go       # 键码 ↔ 键名映射
│   ├── config/
│   │   ├── config.go        # YAML 加载与校验
│   │   └── reload.go        # SIGHUP 热重载
│   ├── matcher/
│   │   └── matcher.go       # 组合键匹配状态机
│   ├── executor/
│   │   └── executor.go      # 命令执行 + 降权
│   └── daemon/
│       └── daemon.go        # 主循环、信号处理
├── configs/
│   └── config.example.yaml  # 配置示例
├── systemd/
│   └── gohotkey.service     # systemd unit 文件
└── scripts/
    ├── install.sh           # 安装脚本
    └── gen_keycodes.sh      # 从内核头文件生成键码表
```

## 四、关键技术决策

### 4.1 为什么选 evdev 而非 X11 / Wayland API

| 方案 | 跨显示服务器 | 实现复杂度 | 权限要求 |
|------|-------------|-----------|---------|
| X11 `XGrabKey` | ✗ 仅 X11 | 低 | 普通用户 |
| Wayland portal | ✗ 仅部分 DE | 高（每个 DE 适配） | 普通用户 |
| **evdev** | ✓ 全场景 | 中 | root 或 input 组 |

evdev 是唯一能在 X11、Wayland、纯 TTY 下都可用的方案，且不依赖任何 DE。

### 4.2 为什么不用 CGO

- **可移植性：** 纯 Go 可交叉编译，单二进制无 glibc 依赖
- **足够用：** evdev 接口可用 `syscall` + `unsafe` 直接读写，不需要 `libevdev` 的高级封装
- **构建简单：** `go build` 一行命令即可

### 4.3 为什么用 YAML 而非 TOML / JSON

- YAML 支持注释和多行字符串，配置可读性最好
- 国内运维同学对 YAML 接受度高（Ansible、Kubernetes 都用 YAML）
- Go 生态 `gopkg.in/yaml.v3` 成熟稳定

### 4.4 为什么按键释放不触发

只在 `Value == 1`（按下）时触发命令。原因：

- 防止 `keyup` 触发导致一次按键执行两次
- 自动重复（`Value == 2`）也不触发，避免长按时疯狂触发

## 五、依赖清单

| 依赖 | 版本 | 用途 |
|------|------|------|
| `gopkg.in/yaml.v3` | v3.0.1 | YAML 配置解析 |
| `github.com/fsnotify/fsnotify` | v1.7.0 | 监听 `/dev/input/` 热插拔 |
| `github.com/sirupsen/logrus` | v1.9.3 | 结构化日志（可选） |

无 CGO 依赖，无 C 库依赖。

## 六、部署方案

### 6.1 安装步骤

```bash
# 1. 编译
go build -o gohotkey ./cmd/gohotkey

# 2. 安装二进制
sudo install -m 755 gohotkey /usr/local/bin/

# 3. 安装配置
sudo mkdir -p /etc/gohotkey
sudo install -m 644 configs/config.example.yaml /etc/gohotkey/config.yaml

# 4. 安装 systemd unit
sudo install -m 644 systemd/gohotkey.service /etc/systemd/system/

# 5. 启用并启动
sudo systemctl daemon-reload
sudo systemctl enable --now gohotkey
```

### 6.2 systemd unit 文件

```ini
[Unit]
Description=Global Hotkey Daemon
After=multi-user.target

[Service]
Type=simple
ExecStart=/usr/local/bin/gohotkey --config /etc/gohotkey/config.yaml
ExecReload=/bin/kill -HUP $MAINPID
Restart=on-failure
RestartSec=5s

# 安全加固
NoNewPrivileges=false  # 必须 false，因为要切换用户
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=true

[Install]
WantedBy=multi-user.target
```

## 七、测试方案

### 7.1 单元测试

- `evdev/event_test.go`：input_event 结构体的二进制解析正确性
- `matcher/matcher_test.go`：组合键匹配逻辑、modifier 状态机
- `config/config_test.go`：YAML 加载、键名解析、错误提示

### 7.2 集成测试

- 使用 `uinput` 创建虚拟键盘，注入按键事件，验证命令是否被触发
- 测试热插拔：动态创建 / 销毁 uinput 设备，验证 daemon 是否正确响应

### 7.3 手工验证

按 [REQUIREMENTS.md](./REQUIREMENTS.md) 第七节的「验收标准」逐项验证。

## 八、开发里程碑

| 阶段 | 内容 | 预计耗时 |
|------|------|---------|
| M1 | evdev 设备发现 + 事件读取（裸读不解析组合键） | 1 天 |
| M2 | 配置加载 + 组合键匹配 + 命令执行 | 2 天 |
| M3 | 降权执行 + 环境变量处理（解决 GUI 启动问题） | 1 天 |
| M4 | 热插拔 + 配置热重载 | 1 天 |
| M5 | systemd 集成、安装脚本、文档 | 1 天 |
| M6 | 单元测试、集成测试、Bug 修复 | 2 天 |

合计约 8 个工作日。

## 九、风险与缓解

| 风险 | 影响 | 缓解措施 |
|------|------|---------|
| evdev 按键被双重消费（daemon + DE 同时响应） | 用户体验问题 | 文档提醒用户在 DE 中取消同名快捷键，或使用不冲突的组合键 |
| root 权限导致命令执行风险 | 安全 | 配置文件权限校验、命令降权、不支持配置注入 |
| GUI 程序启动时 `DISPLAY` 未设置 | GUI 命令起不来 | 自动探测当前登录会话的 `DISPLAY`，必要时通过 `loginctl show-session` 拿到 |
| 不同发行版键码差异 | 个别键失效 | 编译期从 `/usr/include/linux/input-event-codes.h` 生成键码表，文档说明 |

## 十、后续演进

本期不做、但架构上保留扩展点的功能：

- **多键序列（chord）：** matcher 已设计为状态机，扩展为 trie 即可
- **条件触发：** 例如「仅当某窗口聚焦时生效」，可通过插件机制接入
- **IPC 接口：** 通过 Unix socket 让其他程序动态注册 / 取消热键
