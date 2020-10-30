package gobot

import (
	"fmt"
	"time"

	"gobot.io/x/gobot"
	"gobot.io/x/gobot/platforms/ble"
)

const (
	fiioM5ID    = "40-ED-98-1A-25-E4"
	macbookID   = "F8-FF-C2-62-D6-6A"
	macbookName = "KB-20A3KC61J1"
)

//Connect open connection with specific bluetooth low enegy device
func Connect() {
	bleAdaptor := ble.NewClientAdaptor(fiioM5ID)
	battery := ble.NewBatteryDriver(bleAdaptor)

	work := func() {
		gobot.Every(5*time.Second, func() {
			fmt.Println("Battery level:", battery.GetBatteryLevel())
		})
	}

	robot := gobot.NewRobot("bleBot",
		[]gobot.Connection{bleAdaptor},
		[]gobot.Device{battery},
		work,
	)

	robot.Start()
}
