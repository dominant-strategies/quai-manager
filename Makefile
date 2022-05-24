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

# to run auto-miner without providing a location manually
# make sure config.yaml file is set up properly
run:
	./build/bin/manager 0

# to manually select a location to mine
run-mine:
	./build/bin/manager $(region) $(zone) 1

# to run in the background (this will run in auto-miner mode)
run-background:
ifeq (,$(wildcard logs))
	mkdir logs
endif
	@nohup ./build/bin/manager 0 >> logs/manager.log 2>&1 &

# to run in the background (manually set location)
run-mine-background:
ifeq (,$(wildcard logs))
	mkdir logs
endif
	@nohup ./build/bin/manager $(region) $(zone) 1 >> logs/manager.log 2>&1 &


stop:
	@if pgrep -f ./build/bin/manager; then pkill -f ./build/bin/manager; fi
	@echo "Stopping all instances of quai-manager"