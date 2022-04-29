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

### Auto-miner mode

With the introduction of the auto-miner enhancement, it is now possible to let the manager automatically find and set itself to the best location. In this mode, the manager will start at the best location. There is an also an option to "optimize," and if set true it will also check periodically whether it is still in the best location and, if not, it will update to the best location. The best location is the chain with the lowest observed difficulty, meaning in auto-miner mode the manager automatically selects the chain likely to bring the best rewards to a miner while also automatically distributing hashrate across the network evenly.

## Configuring the Manager

### The config.yaml file

In the file config.yaml you should see something like this:

```
# Config for node URLs
Location: [1,1]
Auto: true
Mine: true
Optimize: false
OptimizeTimer: 10
PrimeURL: "url"
RegionURLs: "urls"
ZoneURLs: "urls"
```

This file is responsible for storing your settings. The settings saved in this file on starting the manager are what will be applied when it runs.

Location: this stores the Region and Zone values for setting the mining location manually. (Will only be used if Optimize is set to false.) Values must correspond to the current Quai Network Ontology. At mainnet launch, the values for Region will be 1-3 and for Zone 1-3. So, for example, to mine on Region 2 Zone 3 you would save the Location value like this:

```
Location: [2,3]
```

Auto: if true, then the miner will automatically find and select the best location on start up. If set to false and a location is not provided via arguments the location will default to Location set in config.yaml.

Mine: if true, the miner will mine. This value must be set true in order to mine. If it is set false, then the manager will not mine (though it will perform other functions, such as subscribing to the chains so it will stay updated).

Optimize: if true, then periodically the manager will check the difficulty of the chains and, if a chain with lower difficulty is found, it will switch to that location. If set to false, it will only mine at the location set at startup. (Note: this feature is part of auto-miner mode. During manual mining this will be ignored.)

OptimizeTimer: this value represents how many minutes between Optimize checks the manager will make. By default the value is set to 10.

PrimeURL: stores the URL for the Prime chain. Should not be changed.

RegionURLs: stores the URLs for the Region chains. Should not be changed.

ZoneURLs: stores the URLs for the Zone chains. Should not be changed.

Note that some of the values supplied in the config.yaml file can be overridden with the appropriate command and arguments.

## Run the manager

### Setting the region and zone flags for mining location

The below command runs the manager in auto-miner mode:

Run via Makefile

```
make run
```

When the manager starts it should print something like:

```
Auto-miner mode started with Optimizer=false and timer set to 10 minutes
```

The manager can also be run in the background with logs saved to a file. It can be run similarly to make run, with the same auto-miner and optimizer enhancements possible.

To run in the background:

```
make run-background
```

It is also possible to manually set the location. Doing so will override the location value set in the config.yaml file. The appropriate arguments must also be supplied like this:

```
make run-mine region=1 zone=2
```

This will start the manager mining in Region 1 Zone 2. Location values must correspond to the current Quai Network ontology - at start, that ontology is 3x3, meaning Region values are 1-3 and Zone values are 1-3. So, for example, to mine in Region 3 Zone 1 you would enter:

```
make run-mine region=3 zone=1
```

If you start in manual mode the manager should print:

```
Manual mode started
```

Finally, if Auto is set to false in config.yaml and a location isn't provided through run-mine, then the manager will select the location value stored in config.yaml.

Note! If you enter the arguments wrong the result might be the manager starting in auto-miner mode! The below might start the manager in auto-miner mode:

```
make run-mine regio=1 zone=2
```

So be careful that you've entered your commands and arguments correctly and the manager is showing the expected behavior!


### Set

Run via Go binary

```shell
./build/bin/manager 1 2
```

## Stopping the manager

```shell
make stop
```
