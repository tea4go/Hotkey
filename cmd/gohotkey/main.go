package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/autohotkey/gohotkey/internal/evdev"
)

func main() {
	if len(os.Args) < 2 || os.Args[1] != "debug-evdev" {
		fmt.Fprintf(os.Stderr, "用法: %s debug-evdev\n", os.Args[0])
		os.Exit(1)
	}

	if err := runDebugEvdev(); err != nil {
		fmt.Fprintf(os.Stderr, "错误: %v\n", err)
		os.Exit(1)
	}
}

func runDebugEvdev() error {
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	devices, err := evdev.Discover()
	if err != nil {
		return err
	}

	if len(devices) == 0 {
		fmt.Println("[discover] 未发现键盘设备")
		return nil
	}

	fmt.Printf("[discover] 找到 %d 个键盘设备：\n", len(devices))
	for _, d := range devices {
		fmt.Printf("  - %s  %s\n", d.Path, d.Name)
	}
	fmt.Println()

	var wg sync.WaitGroup
	for _, d := range devices {
		wg.Add(1)
		go func(dev *evdev.Device) {
			defer wg.Done()
			for ev := range dev.Events() {
				action := "?"
				switch ev.Value {
				case 0:
					action = "释放"
				case 1:
					action = "按下"
				case 2:
					action = "重复"
				}
				fmt.Printf("[event] %s (%s) code=%d value=%d (%s)\n",
					dev.Path, dev.Name, ev.Code, ev.Value, action)
			}
		}(d)
	}

	<-ctx.Done()
	fmt.Printf("\n[shutdown] 关闭 %d 个设备\n", len(devices))
	for _, d := range devices {
		d.Close()
	}
	wg.Wait()

	return nil
}
