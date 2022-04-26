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

With the introduction of the auto-miner enhancement, it is now possible to let the manager automatically find and set itself to the best location. In this mode, the manager will start at the best location and also check every 10 minutes whether it is still in the best location and, if not, it will update to the best location. The best location is the chain with the lowest observed difficulty, meaning the auto-miner automatically selects the chain likely to bring the best rewards to a miner while also automatically distributing hashrate across the network evenly.

The below command runs the manager in auto-miner mode:

Run via Makefile

```
make run
```

If preferred, it is still possible to manually set the mining location. It is as simple as providing the arguments to tell the manager what location to select. In manual mode, the miner will not update its mining location but will only mine in the selected location.

The below commands will run the manager in region 1 and zone 2.

It can be set to any value between 1 and 3 for regions and zones and the manager will start in that location.

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

## Stopping the manager

```shell
make stop
```
