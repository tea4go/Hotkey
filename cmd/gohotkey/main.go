package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xproto"
)

func main() {
	conn, err := xgb.NewConn()
	if err != nil {
		log.Fatal(err)
	}
	defer conn.Close()

	setup := xproto.Setup(conn)
	root := setup.DefaultScreen(conn).Root

	// Grab Ctrl+Shift+D (keycode 40 is 'd' on most layouts)
	// Modifiers: Shift=1, Control=4
	modMask := uint16(xproto.ModMaskShift | xproto.ModMaskControl)

	// Grab key 40 (D key) with Ctrl+Shift
	err = xproto.GrabKeyChecked(conn, true, root, modMask, 40,
		xproto.GrabModeAsync, xproto.GrabModeAsync).Check()
	if err != nil {
		log.Fatalf("Failed to grab Ctrl+Shift+D: %v", err)
	}

	fmt.Println("监听 Ctrl+Shift+D，按该组合键启动终端...")

	for {
		ev, err := conn.WaitForEvent()
		if err != nil {
			log.Fatal(err)
		}

		switch e := ev.(type) {
		case xproto.KeyPressEvent:
			if e.Detail == 40 {
				fmt.Println("检测到 Ctrl+Shift+D，启动终端...")
				go runCommand()
			}
		}
	}
}

func runCommand() {
	terminals := []string{"deepin-terminal", "alacritty", "gnome-terminal", "xfce4-terminal", "xterm"}

	for _, term := range terminals {
		cmd := exec.Command(term)
		cmd.Env = append(os.Environ(), "DISPLAY="+os.Getenv("DISPLAY"))
		err := cmd.Start()
		if err == nil {
			fmt.Printf("✓ 启动了 %s\n", term)
			return
		}
	}

	fmt.Println("✗ 未找到可用的终端程序")
}
