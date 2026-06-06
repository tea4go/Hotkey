# M1 规格：evdev 设备发现 + 事件读取

## 目标

实现 evdev 设备发现与事件读取，产出可运行的 `gohotkey debug-evdev` 子命令，实时打印所有键盘设备的按键事件（原始键码）。

## 范围

### 做

- 扫描 `/dev/input/eventN` 并筛选出键盘设备
- 解析 24 字节的 `input_event` 二进制结构
- 每个键盘设备一个 goroutine，持续读取按键事件并经 channel 投递
- 提供 `gohotkey debug-evdev` 子命令观察事件流

### 不做（留给后续里程碑）

| 功能 | 里程碑 |
|------|--------|
| 键码 ↔ 键名映射表 | M2 |
| 组合键匹配 / modifier 状态机 | M2 |
| YAML 配置加载 | M2 |
| 命令执行 + 降权 | M3 |
| 热插拔监听 (inotify) | M4 |
| SIGHUP 配置热重载 | M4 |
| systemd 集成 | M5 |
| 单元测试 / 集成测试 | M6 |

## 模块结构

```
autohotkey/
├── go.mod
├── go.sum
├── cmd/
│   └── gohotkey/
│       └── main.go        — CLI 入口 + debug-evdev 子命令
└── internal/
    └── evdev/
        ├── event.go       — InputEvent 二进制解析
        ├── device.go      — Device 结构体、Events()、Close()
        └── discover.go    — 设备扫描与能力检测
```

## 接口设计

### Event (event.go)

```go
package evdev

const (
    EvSyn uint16 = 0x00
    EvKey uint16 = 0x01
)

// Event 是给下游消费的按键事件（仅 EV_KEY 类型）
type Event struct {
    DevicePath string // 来源设备路径，如 /dev/input/event3
    Type       uint16
    Code       uint16 // 键码，如 30 (KEY_A)
    Value      int32  // 0=释放, 1=按下, 2=自动重复
}

// parseInputEvent 从 24 字节 buffer 解析内核 input_event
// 布局（amd64 小端）：
//   [0:16]  struct timeval { time_t tv_sec; suseconds_t tv_usec; }
//   [16:18] __u16 type
//   [18:20] __u16 code
//   [20:24] __s32 value
func parseInputEvent(buf []byte) (typ, code uint16, value int32)
```

### Device (device.go)

```go
type Device struct {
    Path string  // /dev/input/eventN
    Name string  // 从 EVIOCGNAME 获取的设备名

    fd     *os.File
    events chan Event
    closed chan struct{}
}

// Events 返回该设备的事件 channel。
// 在 Close 调用或读取出错（设备拔出）后，channel 会被关闭。
func (d *Device) Events() <-chan Event

// Close 关闭设备 fd 并停止读取 goroutine。可重复调用。
func (d *Device) Close() error
```

### Discover (discover.go)

```go
// Discover 扫描 /dev/input/ 下所有 eventN 设备，返回所有判定为键盘的设备。
// 已打开 fd 并启动了后台读取 goroutine，调用方负责 Close。
// 当所有设备都打不开（通常是权限问题）时返回错误。
func Discover() ([]*Device, error)
```

## 关键实现细节

### 1. input_event 二进制布局

内核 `struct input_event`（`linux/input.h`）在 amd64 下占 24 字节：

```
偏移   字段        类型              字节数
0      tv_sec      __kernel_long_t   8
8      tv_usec     __kernel_long_t   8
16     type        __u16             2
18     code        __u16             2
20     value       __s32             4
```

M1 不消费时间戳字段，仅解析 type/code/value。

### 2. ioctl 调用（无 CGO）

通过 `golang.org/x/sys/unix.IoctlGetInt` / `unix.Syscall` 直接发起 ioctl。需要的请求码：

| 名称 | 含义 | 备注 |
|------|------|------|
| `EVIOCGNAME(len)` | 读取设备名 | `_IOC(_IOC_READ, 'E', 0x06, len)` |
| `EVIOCGBIT(EV_KEY, len)` | 读取 EV_KEY 能力位图 | `_IOC(_IOC_READ, 'E', 0x20 + EV_KEY, len)` |

`_IOC` 宏在 Go 中等价为：

```go
const (
    iocNRBits   = 8
    iocTypeBits = 8
    iocSizeBits = 14
    iocDirBits  = 2

    iocNRShift   = 0
    iocTypeShift = iocNRShift + iocNRBits      // 8
    iocSizeShift = iocTypeShift + iocTypeBits  // 16
    iocDirShift  = iocSizeShift + iocSizeBits  // 30

    iocRead = 2
)

func ioc(dir, typ, nr, size uintptr) uintptr {
    return (dir << iocDirShift) | (typ << iocTypeShift) | (nr << iocNRShift) | (size << iocSizeShift)
}
```

### 3. 键盘判定规则

通过 `EVIOCGBIT(EV_KEY, ...)` 读到一段 bitmap（最长 `KEY_MAX/8 = 96` 字节就够，M1 用 96 字节）。

字母键在 `input-event-codes.h` 中按 QWERTY 行物理布局排列，并非连续：

| 行 | 键码 |
|----|------|
| Q W E R T Y U I O P | 16 ~ 25 |
| A S D F G H J K L   | 30 ~ 38 |
| Z X C V B N M       | 44 ~ 50 |

判定为键盘的条件：

> 上述 26 个字母键码中，至少有 20 个在 bitmap 中被置位。

阈值取 20 而非 26：键盘布局以外的键码相同（dvorak/colemak 仅影响 keymap，不影响 evdev keycode），保留余量避免误把仅含少量按键的设备（电源按钮、音量旋钮、笔记本上 Fn 派生的伪键盘节点）当成主键盘。

### 4. 读取循环

每个 Device 启动一个 goroutine：

```go
buf := make([]byte, 24)
for {
    _, err := io.ReadFull(d.fd, buf)
    if err != nil {
        close(d.events)
        return
    }
    typ, code, value := parseInputEvent(buf)
    if typ != EvKey {
        continue
    }
    select {
    case d.events <- Event{DevicePath: d.Path, Type: typ, Code: code, Value: value}:
    case <-d.closed:
        close(d.events)
        return
    }
}
```

M1 不做 channel 缓冲优化，使用默认无缓冲 channel。

### 5. debug-evdev 子命令

```
$ sudo gohotkey debug-evdev
[discover] 找到 2 个键盘设备：
  - /dev/input/event3  AT Translated Set 2 keyboard
  - /dev/input/event5  Logitech USB Keyboard

[event] /dev/input/event3 (AT Translated Set 2 keyboard) type=1 code=30 value=1
[event] /dev/input/event3 (AT Translated Set 2 keyboard) type=1 code=30 value=0
^C
[shutdown] 关闭 2 个设备
```

实现要点：
- `main.go` 解析子命令：仅支持 `debug-evdev`，其他参数打印 usage
- 用 `signal.NotifyContext(ctx, SIGINT, SIGTERM)` 捕获 Ctrl+C
- fan-in：为每个 Device 起一个 goroutine 读取其 channel 转发到 stdout
- 收到信号后遍历 `Close()` 所有设备

## 错误处理

| 场景 | 处理 |
|------|------|
| 单个 `/dev/input/eventN` 打开失败 | 跳过该设备，debug-evdev 用 stderr 打印 warning，继续扫描其他 |
| 所有设备都打不开 | `Discover` 返回错误，main.go 提示「请以 root 运行或加入 input 组」 |
| 找到 0 个键盘 | `Discover` 不返错，返回空切片；main.go 打印「未发现键盘设备」并退出码 0 |
| 读取 goroutine 中 read 出错（拔出） | 关闭 events channel 退出，M1 阶段 main 端发现 channel 关闭就当该设备结束 |
| ioctl 失败（设备能力查询） | 跳过该设备 |

M1 不引入 logrus，stderr 直接 `fmt.Fprintf` 输出诊断信息即可。

## 依赖清单

```
go 1.21
golang.org/x/sys v0.x.x  // ioctl、unix 系统调用常量
```

暂不引入：`yaml.v3`（M2 用）、`fsnotify`（M4 用）、`logrus`（M5 之后再评估）。

## 验收方式

M1 阶段无自动化测试，手工验证：

1. `go build ./cmd/gohotkey` 成功
2. `sudo ./gohotkey debug-evdev` 正确列出本机键盘设备
3. 在终端中按下任意键，stdout 实时输出对应的 `type=1 code=N value=1/0` 事件行
4. Ctrl+C 后所有设备正常关闭，无 goroutine 泄漏（用 `pkill -SIGQUIT` 看堆栈确认）
5. 非 root / 非 input 组用户运行时，给出可读的错误提示

## 后续里程碑衔接点

- **M2 复用**：matcher 包按 Device 粒度消费 `Device.Events()`；keycode 包提供 `Code → Name` 与 `Name → Code` 表
- **M4 复用**：discover.go 中扫描逻辑抽出 `openEventDevice(path)` 函数供 inotify watcher 复用
