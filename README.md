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

## Run the manager

### Set the region and zone flags

The below commands will run the manager in region 1 and zone 2.

It can be set to any value between 1 and 3 for regions and zones and the manager will start in that location.

Run via Makefile

```shell
make run region=1 zone=2
```
To run the manager in background and save the logs
```
make run-background region=1 zone=2
```

### Set

Run via Go binary

```shell
./build/bin/manager 1 2
```
