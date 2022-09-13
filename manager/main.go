package main

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math"
	"math/big"
	"math/rand"
	"os"
	"runtime"
	"strconv"
	"sync"
	"time"

	"github.com/TwiN/go-color"
	"github.com/spruce-solutions/go-quai/common"
	"github.com/spruce-solutions/go-quai/common/hexutil"
	"github.com/spruce-solutions/go-quai/consensus/blake3"
	"github.com/spruce-solutions/go-quai/core/types"
	"github.com/spruce-solutions/go-quai/crypto"
	"github.com/spruce-solutions/go-quai/ethclient"
	"github.com/spruce-solutions/quai-manager/manager/util"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10
)

var exit = make(chan bool)
var Reset = "\033[0m"
var Red = "\033[31m"
var Green = "\033[32m"
var Yellow = "\033[33m"
var Blue = "\033[34m"
var Purple = "\033[35m"
var Cyan = "\033[36m"
var Gray = "\033[37m"
var White = "\033[97m"

func init() {
	if runtime.GOOS == "windows" {
		Reset = ""
		Red = ""
		Green = ""
		Yellow = ""
		Blue = ""
		Purple = ""
		Cyan = ""
		Gray = ""
		White = ""
	}
}

type Manager struct {
	engine *blake3.Blake3
	config util.Config

	header *types.Header

	orderedBlockClients orderedBlockClients   // will hold all chain URLs and settings in order from prime to zone-3-3
	pendingBlocks       []*types.ReceiptBlock // Current pending blocks of the manager
	lock                sync.Mutex
	location            []byte

	pendingBlockCh chan *types.ReceiptBlock

	updatedCh chan *types.Header
	resultCh  chan *types.Header
	startCh   chan struct{}
	exitCh    chan struct{}
	doneCh    chan bool // channel for updating location

	previousNumber []*big.Int
}

// Block struct to hold all Client fields.
type orderedBlockClients struct {
	primeClient      *ethclient.Client
	primeAvailable   bool
	regionClients    []*ethclient.Client
	regionsAvailable []bool
	zoneClients      [][]*ethclient.Client
	zonesAvailable   [][]bool
}

var exponentialBackoffCeilingSecs int64 = 14400 // 4 hours

func main() {
	config, err := util.LoadConfig("..")
	if err != nil {
		log.Fatal("cannot load config:", err)
	}

	lastUpdatedAt := time.Now()
	attempts := 0

	// errror handling in case any connections failed
	connectStatus := false
	// Get URLs for all chains and set mining bools to represent if online
	// getting clients comes first because manager can poll chains for auto-mine
	allClients := getNodeClients(config)

	for !connectStatus {
		if time.Now().Sub(lastUpdatedAt).Hours() >= 12 {
			attempts = 0
		}

		connectStatus = true
		if !allClients.primeAvailable {
			connectStatus = false
		}
		for _, status := range allClients.regionsAvailable {
			if !status {
				connectStatus = false
			}
		}
		for _, zonesArray := range allClients.zonesAvailable {
			for _, status := range zonesArray {
				if !status {
					connectStatus = false
				}
			}
		}
		lastUpdatedAt = time.Now()
		attempts += 1

		// exponential back-off implemented
		delaySecs := int64(math.Floor((math.Pow(2, float64(attempts)) - 1) * 0.5))
		if delaySecs > exponentialBackoffCeilingSecs {
			delaySecs = exponentialBackoffCeilingSecs
		}

		// should only get here if the ffmpeg record stream process dies
		fmt.Printf("This is attempt %d to connect to all go-quai nodes. Waiting %d seconds and then retrying...\n", attempts, delaySecs)

		time.Sleep(time.Duration(delaySecs) * time.Second)

		allClients = getNodeClients(config)
	}

	if !connectStatus {
		log.Println("Some or all connections to chains not available")
		log.Println("For best performance check your connections and restart the manager")
	}

	// set mining location
	// if using the run-mine command then must remember to set region and zone locations
	// if using run then the manager will automatically follow the chain with lowest difficulty
	if len(os.Args) > 3 {
		location := os.Args[1:3]
		mine, _ := strconv.Atoi(os.Args[3:][0])

		// error management to check correct number of values provided
		if len(location) == 0 {
			log.Fatal("Please mention location where you want to mine")
		}
		if len(location) == 1 {
			log.Fatal("You are missing either Region or Zone location")
		}
		if len(location) > 2 {
			log.Fatal("Only specify 2 values for the location")
		}

		// converting region and zone location values from string to integer
		regionLoc, _ := strconv.Atoi(location[0])
		zoneLoc, _ := strconv.Atoi(location[1])

		// converting region and zone integer values to bytes
		RegionLocArr := make([]byte, 8)
		ZoneLocArr := make([]byte, 8)
		binary.LittleEndian.PutUint64(RegionLocArr, uint64(regionLoc))
		binary.LittleEndian.PutUint64(ZoneLocArr, uint64(zoneLoc))

		config.Location = []byte{RegionLocArr[0], ZoneLocArr[0]}
		config.Mine = mine == 1
		log.Println(color.Ize(color.Red, "Manual mode started"))
	}

	header := &types.Header{
		ParentHash:        make([]common.Hash, 3),
		Number:            make([]*big.Int, 3),
		Extra:             make([][]byte, 3),
		Time:              uint64(0),
		BaseFee:           make([]*big.Int, 3),
		GasLimit:          make([]uint64, 3),
		Coinbase:          make([]common.Address, 3),
		Difficulty:        make([]*big.Int, 3),
		NetworkDifficulty: make([]*big.Int, 3),
		Root:              make([]common.Hash, 3),
		TxHash:            make([]common.Hash, 3),
		UncleHash:         make([]common.Hash, 3),
		ReceiptHash:       make([]common.Hash, 3),
		GasUsed:           make([]uint64, 3),
		Bloom:             make([]types.Bloom, 3),
	}

	blake3Config := blake3.Config{
		MiningThreads: 0,
		NotifyFull:    true,
	}

	blake3Engine, err := blake3.New(blake3Config, nil, false)
	if nil != err {
		log.Fatal("Failed to create Blake3 engine: ", err)
	}

	m := &Manager{
		engine:              blake3Engine,
		orderedBlockClients: allClients,
		header:              header,
		pendingBlocks:       make([]*types.ReceiptBlock, 3),
		pendingBlockCh:      make(chan *types.ReceiptBlock, resultQueueSize),
		resultCh:            make(chan *types.Header, resultQueueSize),
		updatedCh:           make(chan *types.Header, resultQueueSize),
		exitCh:              make(chan struct{}),
		startCh:             make(chan struct{}, 1),
		doneCh:              make(chan bool),
		location:            config.Location,
		previousNumber:      make([]*big.Int, 3),
		config:              config,
	}

	m.previousNumber = []*big.Int{big.NewInt(0), big.NewInt(0), big.NewInt(0)}

	if config.Mine {
		log.Println("Starting manager in location ", config.Location)

		m.fetchPendingHeader(m.orderedBlockClients.zoneClients[m.location[0]-1][m.location[1]-1])

		// subscribing to the zone pending header update.
		m.subscribeSlicePendingHeader()

		m.subscribeSliceHeaderRoots()

		go m.resultLoop()

		go m.miningLoop()

		go m.SubmitHashRate()
	}
	<-exit
}

// getNodeClients takes in a config and retrieves the Prime, Region, and Zone client
// that is used for mining in a slice.
func getNodeClients(config util.Config) orderedBlockClients {

	// initializing all the clients
	allClients := orderedBlockClients{
		primeAvailable:   false,
		regionClients:    make([]*ethclient.Client, 3),
		regionsAvailable: make([]bool, 3),
		zoneClients:      make([][]*ethclient.Client, 3),
		zonesAvailable:   make([][]bool, 3),
	}

	for i := range allClients.zoneClients {
		allClients.zoneClients[i] = make([]*ethclient.Client, 3)
	}
	for i := range allClients.zonesAvailable {
		allClients.zonesAvailable[i] = make([]bool, 3)
	}

	// add Prime to orderedBlockClient array at [0]
	if config.PrimeURL != "" {
		primeClient, err := ethclient.Dial(config.PrimeURL)
		if err != nil {
			log.Println("Unable to connect to node:", "Prime", config.PrimeURL)
		} else {
			allClients.primeClient = primeClient
			allClients.primeAvailable = true
		}
	}

	// loop to add Regions to orderedBlockClient
	// remember to set true value for Region to be mined
	for i, regionURL := range config.RegionURLs {
		if regionURL != "" {
			regionClient, err := ethclient.Dial(regionURL)
			if err != nil {
				log.Println("Unable to connect to node:", "Region", i+1, regionURL)
				allClients.regionsAvailable[i] = false
			} else {
				allClients.regionsAvailable[i] = true
				allClients.regionClients[i] = regionClient
			}
		}
	}

	// loop to add Zones to orderedBlockClient
	// remember ZoneURLS is a 2D array
	for i, zonesURLs := range config.ZoneURLs {
		for j, zoneURL := range zonesURLs {
			if zoneURL != "" {
				zoneClient, err := ethclient.Dial(zoneURL)
				if err != nil {
					log.Println("Unable to connect to node:", "Zone", i+1, j+1, zoneURL)
					allClients.zonesAvailable[i][j] = false
				} else {
					allClients.zonesAvailable[i][j] = true
					allClients.zoneClients[i][j] = zoneClient
				}
			}
		}
	}
	return allClients
}

// subscribePendingHeader subscribes to the head of the mining nodes in order to pass
// the most up to date block to the miner within the manager.
func (m *Manager) subscribePendingHeader(client *ethclient.Client, sliceIndex int) {
	log.Println("Current location is ", m.location)
	// check the status of the sync
	checkSync, err := client.SyncProgress(context.Background())

	if err != nil {
		switch sliceIndex {
		case 0:
			log.Println("Error occured while synching to Prime")
		case 1:
			log.Println("Error occured while synching to Region")
		case 2:
			log.Println("Error occured while synching to Zone")
		}
	}

	// wait until sync is nil to continue
	for checkSync != nil && err == nil {
		time.Sleep(time.Duration(1) * time.Second)
		checkSync, err = client.SyncProgress(context.Background())
		if err != nil {
			log.Println("error during syncing: ", err, checkSync)
		}
	}

	// done channel in case best Location updates
	// subscribe to the pending block only if not synching
	if checkSync == nil && err == nil {
		// Wait for chain events and push them to clients
		header := make(chan *types.Header)
		sub, err := client.SubscribePendingHeader(context.Background(), header)
		if err != nil {
			log.Fatal("Failed to subscribe to pending block events", err)
		}
		defer sub.Unsubscribe()

		// Wait for various events and passing to the appropriate background threads
		for {
			select {
			case m.header = <-header:
				m.updatedCh <- m.header
				// New head arrived, send if for state update if there's none running
			case <-m.doneCh: // location updated and this routine needs to be stopped to start a new one
				break
			}
		}
	}
}

// PendingBlocks gets the latest block when we have received a new pending header. This will get the receipts,
// transactions, and uncles to be stored during mining.
func (m *Manager) fetchPendingHeader(client *ethclient.Client) {
	var header *types.Header
	var err error

	m.lock.Lock()
	header, err = client.GetPendingHeader(context.Background())

	// retrying for 5 times if pending block not found
	if err != nil || header == nil {
		log.Println("Pending block not found error:", err)
		found := false
		attempts := 0
		lastUpdatedAt := time.Now()

		for !found {
			if time.Now().Sub(lastUpdatedAt).Hours() >= 12 {
				attempts = 0
			}

			header, err = client.GetPendingHeader(context.Background())
			if err == nil && header != nil {
				break
			}
			lastUpdatedAt = time.Now()
			attempts += 1

			// exponential back-off implemented
			delaySecs := int64(math.Floor((math.Pow(2, float64(attempts)) - 1) * 0.5))
			if delaySecs > exponentialBackoffCeilingSecs {
				delaySecs = exponentialBackoffCeilingSecs
			}

			// should only get here if the ffmpeg record stream process dies
			fmt.Printf("This is attempt %d to fetch pending block. Waiting %d seconds and then retrying...\n", attempts, delaySecs)

			time.Sleep(time.Duration(delaySecs) * time.Second)
		}
	}

	m.lock.Unlock()
	m.updatedCh <- header
}

// check if the header is null. If so, don't start mining.
func (m *Manager) headerNullCheck(header *types.Header) error {
	err := errors.New("header has nil value, cannot continue with mining")
	if header.Number[0] == nil {
		log.Println("Waiting to retrieve Prime header information...")
		return err
	}
	if header.Number[1] == nil {
		log.Println("Waiting to retrieve Region header information...")
		return err
	}
	if header.Number[2] == nil {
		log.Println("Waiting to retrieve Zone header information...")
		return err
	}
	return nil
}

// miningLoop iterates on a new header and passes the result to m.resultCh. The result is called within the method.
func (m *Manager) miningLoop() error {
	var (
		stopCh chan struct{}
	)
	// interrupt aborts the in-flight sealing task.
	interrupt := func() {
		if stopCh != nil {
			close(stopCh)
			stopCh = nil
		}
	}
	for {
		select {
		case header := <-m.updatedCh:
			// Mine the header here
			// Return the valid header with proper nonce and mix digest
			// Interrupt previous sealing operation
			interrupt()
			stopCh = make(chan struct{})
			// See if we can grab the lock in order to start mining
			// Lock should be held while sending mined blocks
			// Reduce race conditions while sending mined blocks and waiting for pending headers
			m.lock.Lock()
			m.lock.Unlock()

			if !bytes.Equal(header.Location, m.location) {
				continue
			}
			headerNull := m.headerNullCheck(header)
			if headerNull == nil {
				if header.Number[0].Cmp(m.previousNumber[0]) != 0 || header.Number[1].Cmp(m.previousNumber[1]) != 0 || header.Number[2].Cmp(m.previousNumber[2]) != 0 {
					if header.Number[0].Cmp(m.previousNumber[0]) != 0 {
						log.Println("Mining Block:  ", fmt.Sprintf("[%s %s %s]", color.Ize(color.Red, fmt.Sprint(header.Number[0])), header.Number[1].String(), header.Number[2].String()), "location", m.location, "difficulty", header.Difficulty)
					} else if header.Number[1].Cmp(m.previousNumber[1]) != 0 {
						log.Println("Mining Block:  ", fmt.Sprintf("[%s %s %s]", header.Number[0].String(), color.Ize(color.Yellow, fmt.Sprint(header.Number[1])), header.Number[2].String()), "location", m.location, "difficulty", header.Difficulty)
					} else {
						log.Println("Mining Block:  ", header.Number, "location", m.location, "difficulty", header.Difficulty)

					}
					m.previousNumber = header.Number
				}

				if err := m.engine.SealHeader(header, m.resultCh, stopCh); err != nil {
					log.Println("Block sealing failed", "err", err)
				}
			}
		}
	}
}

// WatchHashRate is a simple method to watch the hashrate of our miner and log the output.
func (m *Manager) SubmitHashRate() {
	ticker := time.NewTicker(60 * time.Second)

	// generating random ID to submit in the SubmitHashRate method
	randomId := rand.Int()
	randomIdArray := make([]byte, 8)
	binary.LittleEndian.PutUint64(randomIdArray, uint64(randomId))
	id := crypto.Keccak256Hash(randomIdArray)

	var null float64 = 0
	go func() {
		for {
			select {
			case <-ticker.C:
				hashRate := m.engine.Hashrate()
				if hashRate != null {
					log.Println("Quai Miner  :   Hashes per second: ", hashRate)
					m.engine.SubmitHashrate(hexutil.Uint64(hashRate), id)
				}
			}
		}
	}()
}

// resultLoop takes in the result and passes to the proper channels for receiving.
func (m *Manager) resultLoop() error {
	for {
		select {
		case header := <-m.resultCh:
			m.lock.Lock()

			context, err := m.engine.GetDifficultyOrder(header)
			if err != nil {
				log.Println("Block mined has an invalid order")
			}

			if context == 0 {
				log.Println(color.Ize(color.Red, "PRIME block :  "), header.Number, header.Hash())
			}

			if context == 1 {
				log.Println(color.Ize(color.Yellow, "REGION block:  "), header.Number, header.Hash())
			}

			if context == 2 {
				log.Println(color.Ize(color.Blue, "Zone block  :  "), header.Number, header.Hash())
			}

			// Check to see that all nodes are running before sending blocks to them.
			if !m.allChainsOnline() {
				log.Println("At least one of the chains is not online at the moment")
				continue
			}

			// Check proper difficulty for which nodes to send block to
			// Notify blocks to put in cache before assembling new block on node
			if context == 0 && header.Number[0] != nil {
				var wg sync.WaitGroup
				wg.Add(1)
				go m.SendMinedHeader(2, header, &wg)
				wg.Add(1)
				go m.SendMinedHeader(1, header, &wg)
				wg.Add(1)
				go m.SendMinedHeader(0, header, &wg)
				wg.Wait()
			}

			// If Region difficulty send to Region
			if context == 1 && header.Number[1] != nil {
				var wg sync.WaitGroup
				wg.Add(1)
				go m.SendMinedHeader(2, header, &wg)
				wg.Add(1)
				go m.SendMinedHeader(1, header, &wg)
				wg.Wait()
			}

			// If Zone difficulty send to Zone
			if context == 2 && header.Number[2] != nil {
				var wg sync.WaitGroup
				wg.Add(1)
				go m.SendMinedHeader(2, header, &wg)
				wg.Wait()
			}
			m.lock.Unlock()
		}
	}
}

// allChainsOnline checks if every single chain is online before sending the mined block to make sure that we don't have
// external blocks not found error
func (m *Manager) allChainsOnline() bool {
	if !checkConnection(m.orderedBlockClients.primeClient) {
		return false
	}
	if !checkConnection(m.orderedBlockClients.regionClients[m.location[0]-1]) {
		regionURL := m.config.RegionURLs[m.location[0]-1]
		if regionURL != "" {
			regionClient, err := ethclient.Dial(regionURL)
			if err != nil {
				log.Println("Unable to connect to node:", "Region ", m.location, regionURL)
				return false
			} else {
				m.orderedBlockClients.regionClients[m.location[0]-1] = regionClient
			}
		}
	}
	if !checkConnection(m.orderedBlockClients.zoneClients[m.location[0]-1][m.location[1]-1]) {
		zoneURL := m.config.ZoneURLs[m.location[0]-1][m.location[1]-1]
		if zoneURL != "" {
			zoneClient, err := ethclient.Dial(zoneURL)
			if err != nil {
				log.Println("Unable to connect to node:", "Region ", m.location[0]-1, "Zone ", m.location[1]-1, zoneURL)
				return false
			} else {
				m.orderedBlockClients.zoneClients[m.location[0]-1][m.location[1]-1] = zoneClient
			}
		}
	}
	return true
}

// SendMinedHeader sends the mined block to its mining client with the transactions, uncles, and receipts.
func (m *Manager) SendMinedHeader(mined int, header *types.Header, wg *sync.WaitGroup) {
	if mined == 0 {
		m.orderedBlockClients.primeClient.ReceiveMinedHeader(context.Background(), header)
	}
	if mined == 1 {
		m.orderedBlockClients.regionClients[m.location[0]-1].ReceiveMinedHeader(context.Background(), header)
	}
	if mined == 2 {
		m.orderedBlockClients.zoneClients[m.location[0]-1][m.location[1]-1].ReceiveMinedHeader(context.Background(), header)
	}
	defer wg.Done()
}

// Checks if a connection is still there on orderedBlockClient.chainAvailable
func checkConnection(client *ethclient.Client) bool {
	_, err := client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Println("Error: connection lost")
		log.Println(err)
		return false
	} else {
		return true
	}
}

func (m *Manager) subscribeSliceHeaderRoots() {
	go m.subscribeHeaderRoots(m.orderedBlockClients.primeClient, 0)
	go m.subscribeHeaderRoots(m.orderedBlockClients.regionClients[m.location[0]-1], 1)
	go m.subscribeHeaderRoots(m.orderedBlockClients.zoneClients[m.location[0]-1][m.location[1]-1], 2)
}

func (m *Manager) subscribeSlicePendingHeader() {
	go m.subscribePendingHeader(m.orderedBlockClients.primeClient, 0)
	go m.subscribePendingHeader(m.orderedBlockClients.regionClients[m.location[0]-1], 1)
	go m.subscribePendingHeader(m.orderedBlockClients.zoneClients[m.location[0]-1][m.location[1]-1], 2)
}

func (m *Manager) subscribeHeaderRoots(client *ethclient.Client, index int) {
	fmt.Println("subscribing to: ", index)
	headerRoots := make(chan types.HeaderRoots, 1)
	// subscribe to the header roots update
	sub, err := client.SubscribeHeaderRoots(context.Background(), headerRoots)
	if err != nil {
		log.Fatal("Failed to subscribe to Header roots event", err)
	}
	defer sub.Unsubscribe()

	// Wait for various events and assining to the appropriate background threads
	for {
		select {
		case headerRoots := <-headerRoots:
			m.lock.Lock()
			m.header.Root[index] = headerRoots.StateRoot
			m.header.TxHash[index] = headerRoots.TxsRoot
			m.header.ReceiptHash[index] = headerRoots.ReceiptsRoot

			// send the updated header roots to mine
			m.updatedCh <- m.header
			m.lock.Unlock()
		case <-m.doneCh: // location updated and this routine needs to be stopped to start a new one
			break
		}
	}
}
