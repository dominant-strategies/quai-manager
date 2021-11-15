# Quai Manager

Official Golang implementation of the Quai Manager.

## Building the source

For prerequisites and detailed build instructions please read the [Installation Instructions](https://docs.quai.network/develop/mining).

Building `quai-manager` requires both a Go (version 1.14 or later) and a C compiler. You can install
them using your favourite package manager. Once the dependencies are installed, run

Build via Makefile
```shell
make quai-manager
```

Build via GoLang directly
```shell
go build -o ./build/bin/manager manager/main.go     
```

## Configure mining endpoints
To configure the mining endpoints in which the manager will pull from:
1. Copy the config.yaml.dist into a config.yaml file.
2. Edit the endpoints to your choosing.
3. Default is set to local node endpoints with default WebSocket endpoints for each node.


## Run the manager

Run via Makefile
```shell
make run
```

Run via Go binary
```shell
./build/bin/manager
```

