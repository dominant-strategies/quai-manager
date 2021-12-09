# This Makefile is meant to be used by people that do not usually work
# with Go source code. If you know what GOPATH is then you probably
# don't need to bother with make.

GOBIN = ./build/bin
GO ?= latest
GORUN = env GO111MODULE=on go run

quai-manager:
	go build -o ./build/bin/manager manager/main.go  
	@echo "Done building."
	@echo "Run \"$(GOBIN)/manager\" to launch quai-manager"

run:
	./build/bin/manager $(region) $(zone)

run-background:
ifeq (,$(wildcard logs))
	mkdir logs
endif
	@nohup ./build/bin/manager $(region) $(zone) >> logs/manager.log 2>&1 &

stop:
	pkill -f './build/bin/manager'