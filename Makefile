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
	./build/bin/manager 0

run-mine:
	./build/bin/manager $(region) $(zone) 1

run-background:
ifeq (,$(wildcard logs))
	mkdir logs
endif
	@nohup ./build/bin/manager $(region) $(zone) 0 >> logs/manager.log 2>&1 &

stop:
	@if pgrep -f ./build/bin/manager; then pkill -f ./build/bin/manager; fi
	@echo "Stopping all instances of quai-manager"

run-mine-background:

ifeq (,$(wildcard logs))
	mkdir logs
endif
	@nohup ./build/bin/manager $(region) $(zone) 1 >> logs/manager.log 2>&1 &