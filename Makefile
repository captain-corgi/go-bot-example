run-bluetooth:
	go run cmd/bluetooth/main.go

run-bluetooth-le:
	GODEBUG=cgocheck=0 go run cmd/bluetooth/main.go

run-keyboard:
	GODEBUG=cgocheck=0 go run cmd/keyboard/main.go