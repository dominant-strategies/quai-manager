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
	"strconv"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/spruce-solutions/go-quai/common"
	"github.com/spruce-solutions/go-quai/common/hexutil"
	"github.com/spruce-solutions/go-quai/consensus/blake3"
	"github.com/spruce-solutions/go-quai/core"
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

type Manager struct {
	engine *blake3.Blake3

	orderedBlockClients orderedBlockClients // will hold all chain URLs and settings in order from prime to zone-3-3
	combinedHeader      *types.Header
	pendingBlocks       []*types.ReceiptBlock // Current pending blocks of the manager
	lock                sync.Mutex
	location            []byte

	pendingPrimeBlockCh  chan *types.ReceiptBlock
	pendingRegionBlockCh chan *types.ReceiptBlock
	pendingZoneBlockCh   chan *types.ReceiptBlock

	updatedCh chan *types.Header
	resultCh  chan *types.HeaderBundle
	startCh   chan struct{}
	exitCh    chan struct{}
	doneCh    chan bool // channel for updating location

	BlockCache [][]*lru.Cache // Cache for the most recent entire blocks
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

func main() {
	config, err := util.LoadConfig("..")
	if err != nil {
		log.Fatal("cannot load config:", err)
	}

	// Get URLs for all chains and set mining bools to represent if online
	// getting clients comes first because manager can poll chains for auto-mine
	allClients := getNodeClients(config)

	// errror handling in case any connections failed
	connectStatus := true
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
	if !connectStatus {
		log.Println("Some or all connections to chains not available")
		log.Println("For best performance check your connections and restart the manager")
	}

	// variable to check whether mining location is set manually or automatically
	var changeLocationCycle bool

	// set mining location
	// if using the run-mine command then must remember to set region and zone locations
	// if using run then the manager will automatically follow the chain with lowest difficulty
	if len(os.Args) > 3 {
		changeLocationCycle = false
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
		fmt.Println("Manual mode started")
	} else {
		if config.Auto && config.Mine { // auto-miner
			config.Location = findBestLocation(allClients)
			config.Mine = true
			changeLocationCycle = config.Optimize
			fmt.Println("Aut-miner mode started with Optimizer= ", config.Optimize, "and timer set to ", config.OptimizeTimer, "minutes")
		} else { // if run
			changeLocationCycle = false
			location := config.Location

			if len(location) != 2 {
				log.Fatal("Only specify 2 values for the location")
				fmt.Println("Make sure to set config.yaml file properly")
			}
			fmt.Println("Listening mode started")
		}
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
		engine:               blake3Engine,
		orderedBlockClients:  allClients,
		combinedHeader:       header,
		pendingBlocks:        make([]*types.ReceiptBlock, 3),
		pendingPrimeBlockCh:  make(chan *types.ReceiptBlock, resultQueueSize),
		pendingRegionBlockCh: make(chan *types.ReceiptBlock, resultQueueSize),
		pendingZoneBlockCh:   make(chan *types.ReceiptBlock, resultQueueSize),
		resultCh:             make(chan *types.HeaderBundle, resultQueueSize),
		updatedCh:            make(chan *types.Header, resultQueueSize),
		exitCh:               make(chan struct{}),
		startCh:              make(chan struct{}, 1),
		doneCh:               make(chan bool),
		location:             config.Location,
	}

	go m.subscribeNewHead()
	go m.subscribeReOrg()

	if config.Mine {
		log.Println("Starting manager in location ", config.Location)

		m.subscribeAllPendingBlocks()

		go m.resultLoop()

		go m.miningLoop()

		go m.SubmitHashRate()

		go m.loopGlobalBlock()

		// fetching the pending blocks
		m.fetchAllPendingBlocks()

		if changeLocationCycle {
			go m.checkBestLocation(config.OptimizeTimer)
		}
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
			log.Println("Error connecting to Prime mining node ", config.PrimeURL)
		} else {
			allClients.primeClient = primeClient
			allClients.primeAvailable = true
		}
	}

	// loop to add Regions to orderedBlockClient
	// remember to set true value for Region to be mined
	for i, URL := range config.RegionURLs {
		regionURL := URL
		if regionURL != "" {
			regionClient, err := ethclient.Dial(regionURL)
			if err != nil {
				log.Println("Error connecting to Region mining node ", URL, " in location ", i)
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
					log.Println("Error connecting to Zone mining node ", zoneURL, " in location ", i, " ", j)
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
		sub, err := client.SubscribePendingBlock(context.Background(), header)
		if err != nil {
			log.Fatal("Failed to subscribe to pending block events", err)
		}
		defer sub.Unsubscribe()

		// Wait for various events and assing to the appropriate background threads
		for {
			select {
			case <-header:
				// New head arrived, send if for state update if there's none running
				m.fetchPendingBlocks(client, sliceIndex)
			case <-m.doneCh: // location updated and this routine needs to be stopped to start a new one
				break
			}
		}
	}
}

// subscribeNewHead passes new head blocks as external blocks to lower level chains.
func (m *Manager) subscribeNewHead() {
	// subscribe to the prime client at context 0
	go m.subscribeNewHeadClient(m.orderedBlockClients.primeClient, 0)
	// subscribe to the region clients
	for i, blockClient := range m.orderedBlockClients.regionClients {
		go m.subscribeNewHeadClient(blockClient, 1)
		for _, zoneBlockClient := range m.orderedBlockClients.zoneClients[i] {
			go m.subscribeNewHeadClient(zoneBlockClient, 2)
		}
	}
}

func (m *Manager) subscribeNewHeadClient(client *ethclient.Client, difficultyContext int) {
	newHeadChannel := make(chan *types.Header, 1)
	retryAttempts := 5
	sub, err := client.SubscribeNewHead(context.Background(), newHeadChannel)
	if err != nil {
		log.Fatal("Failed to subscribe to the new heqad notifications ", err)
	}
	defer sub.Unsubscribe()

	for {
		select {
		case newHead := <-newHeadChannel:
			// get the block and receipt block
			block, err := client.BlockByHash(context.Background(), newHead.Hash())
			if err != nil {
				log.Println("Failed to retrieve block for hash", "hash ", newHead.Hash())

				for i := 0; ; i++ {
					block, err = client.BlockByHash(context.Background(), newHead.Hash())
					if err == nil {
						break
					}

					if i >= retryAttempts {
						log.Println("Failed to retrieve block for hash ", "hash ", newHead.Hash(), " even after ", retryAttempts, " retry attempts ")
						return
					}

					time.Sleep(12 * time.Second)

					log.Println("Retry attempt:", i+1, "Failed to retrieve block for hash ", "hash", newHead.Hash())
				}
			}

			receiptBlock, receiptErr := client.GetBlockReceipts(context.Background(), newHead.Hash())
			if receiptErr != nil {
				log.Println("Failed to retrieve receipts for block", "hash", newHead.Hash())

				for i := 0; ; i++ {
					receiptBlock, receiptErr = client.GetBlockReceipts(context.Background(), newHead.Hash())
					if receiptErr == nil {
						break
					}

					if i >= retryAttempts {
						log.Fatal("Failed to retrieve receipts for block", "hash", newHead.Hash(), " even after ", retryAttempts, " retry attempts ")
					}

					time.Sleep(12 * time.Second)

					log.Println("Retry attempt:", i+1, "Failed to retrieve receipts for block", "hash", newHead.Hash())
				}
			}

			log.Println("Received new head block:", "context", difficultyContext, "location", newHead.Location, "number", newHead.Number, "hash", newHead.Hash())
			if difficultyContext == 0 {
				m.SendClientsExtBlock(difficultyContext, []int{1, 2}, block, receiptBlock)
			} else if difficultyContext == 1 {
				m.SendClientsExtBlock(difficultyContext, []int{0, 2}, block, receiptBlock)
			} else if difficultyContext == 2 {
				m.SendClientsExtBlock(difficultyContext, []int{0, 1}, block, receiptBlock)
			}
		}
	}
}

// subscribe to the reorg notifications from all Prime and Region chians
// subscribeReOrg subscribes to the reOrg events so that we can send the reorg
// information to clients in lower contexts
func (m *Manager) subscribeReOrg() {
	prime := "prime"
	regions := [3]string{"region-1", "region-2", "region-3"}
	// subscribe to the prime and region clients
	// prime is always true so simply directly subscribe
	go m.subscribeReOrgClients(m.orderedBlockClients.primeClient, prime, 0)
	go m.subscribeUncleClients(m.orderedBlockClients.primeClient, prime, 0)

	// subscribe to the regions from external contexts
	for i, client := range m.orderedBlockClients.regionClients {
		go m.subscribeReOrgClients(client, regions[i], 1)
		go m.subscribeUncleClients(client, regions[i], 1)
	}
}

// checkNonceEmpty checks if any of the headers have empty nonce
func checkNonceEmpty(commonHead *types.Header, oldChain, newChain []*types.Header) bool {
	if commonHead.Nonce == (types.BlockNonce{}) {
		return false
	}

	for i := 0; i < len(oldChain); i++ {
		if oldChain[i].Nonce == (types.BlockNonce{}) {
			return false
		}
	}
	for i := 0; i < len(newChain); i++ {
		if newChain[i].Nonce == (types.BlockNonce{}) {
			return false
		}
	}
	return true
}

// compareDifficulty compares 2 chains and returns true if the newChain is heavier
// than the oldChain and false otherwise
func compareReorgDifficulty(commonHead *types.Header, oldChain, newChain []*types.Header, difficultyContext int) bool {

	oldChainDifficulty := big.NewInt(0)
	newChainDifficulty := big.NewInt(0)

	nonceEmpty := checkNonceEmpty(commonHead, oldChain, newChain)

	for i := 0; i < len(oldChain); i++ {
		oldChainDifficulty.Add(oldChainDifficulty, oldChain[i].Difficulty[difficultyContext])
	}
	for i := 0; i < len(newChain); i++ {
		newChainDifficulty.Add(newChainDifficulty, newChain[i].Difficulty[difficultyContext])
	}
	return newChainDifficulty.Cmp(oldChainDifficulty) > 0 && nonceEmpty
}

func (m *Manager) subscribeReOrgClients(client *ethclient.Client, location string, difficultyContext int) {
	reOrgData := make(chan core.ReOrgRollup, 1)
	sub, err := client.SubscribeReOrg(context.Background(), reOrgData)
	if err != nil {
		log.Fatal("Failed to subscribe to the reorg notifications in", location, err)
	}
	defer sub.Unsubscribe()

	for {
		select {
		case reOrgData := <-reOrgData:
			fmt.Println("reorgEvent", reOrgData.ReOrgHeader.Hash().Hex(), location, difficultyContext)

			filteredReOrgData := m.filterReOrgData(reOrgData.OldChainHeaders)
			for location, header := range filteredReOrgData {
				fmt.Println("2", "oldHeader", header.Hash().Hex(), location, difficultyContext)
				m.sendReOrgHeader(header, header.Location, difficultyContext)
			}
		}
	}
}

// filterReOrgData constructs a map to store the rollback point for each region location
func (m *Manager) filterReOrgData(headers []*types.Header) map[string]*types.Header {
	var filteredReOrgData = map[string]*types.Header{}
	// Reverse header list
	for i, j := 0, len(headers)-1; i < j; i, j = i+1, j-1 {
		headers[i], headers[j] = headers[j], headers[i]
	}
	for _, header := range headers {
		_, exists := filteredReOrgData[string(header.Location)]
		// Check if the entry already exists and if the block in the region context is earlier
		// this ensures that we don't send in extra requests during a reorg rollback
		fmt.Println("Exists?", exists, header.Location, header.Hash().Hex())
		if exists {
			continue
		} else {
			filteredReOrgData[string(header.Location)] = header
		}
	}
	return filteredReOrgData
}

func (m *Manager) subscribeUncleClients(client *ethclient.Client, location string, difficultyContext int) {
	uncleEvent := make(chan *types.Header)
	sub, err := client.SubscribeChainUncleEvent(context.Background(), uncleEvent)
	if err != nil {
		log.Fatal("Failed to subscribe to the side event notifications in", location, err)
	}
	defer sub.Unsubscribe()

	for {
		select {
		case uncleEvent := <-uncleEvent:
			fmt.Println("uncleEvent", uncleEvent.Hash(), uncleEvent.Location, location, difficultyContext)
			m.sendReOrgHeader(uncleEvent, uncleEvent.Location, difficultyContext)
		}
	}
}

// getRegionIndex returns the location index of the reorgLocation
func getRegionIndex(location string) int {
	if location == "region-1" {
		return 1
	}
	if location == "region-2" {
		return 2
	}
	if location == "region-3" {
		return 3
	}
	return -1
}

// sendReOrgHeader sends the reorg header to the respective region and zone clients
func (m *Manager) sendReOrgHeader(header *types.Header, location []byte, difficultyContext int) {
	if difficultyContext < 1 {
		// if the reorg happens in a prime context we have to send the reorg rollback
		// to only the affected region and its zones
		regionClient := m.orderedBlockClients.regionClients[location[0]-1]
		go regionClient.SendReOrgData(context.Background(), header)
	}
	if difficultyContext < 2 {
		zoneClient := m.orderedBlockClients.zoneClients[location[0]-1][location[1]-1]
		go zoneClient.SendReOrgData(context.Background(), header)
	}
}

// PendingBlocks gets the latest block when we have received a new pending header. This will get the receipts,
// transactions, and uncles to be stored during mining.
func (m *Manager) fetchPendingBlocks(client *ethclient.Client, sliceIndex int) {
	retryAttempts := 5
	var receiptBlock *types.ReceiptBlock
	var err error

	m.lock.Lock()
	receiptBlock, err = client.GetPendingBlock(context.Background())

	// check for stale headers and refetch the latest header
	if receiptBlock != nil && receiptBlock.Header().Number[sliceIndex] == m.combinedHeader.Number[sliceIndex] && err == nil {
		switch sliceIndex {
		case 0:
			log.Println("Expected header numbers don't match for Prime at block height", receiptBlock.Header().Number[0])
			log.Println("Retrying and attempting to refetch the latest header for Prime")
		case 1:
			log.Println("Expected header numbers don't match for Region at block height", receiptBlock.Header().Number[1])
			log.Println("Retrying and attempting to refetch the latest header for Region")
		case 2:
			log.Println("Expected header numbers don't match for Zone at block height", receiptBlock.Header().Number[2])
			log.Println("Retrying and attempting to refetch the latest header for Zone")
		}
		receiptBlock, err = client.GetPendingBlock(context.Background())
	}

	// retrying for 5 times if pending block not found
	if err != nil {
		log.Println("Pending block not found for index:", sliceIndex, "error:", err)

		for i := 0; ; i++ {
			receiptBlock, err = client.GetPendingBlock(context.Background())
			if err == nil {
				break
			}

			if i >= retryAttempts {
				log.Println("Pending block was never found for index:", sliceIndex, " even after ", retryAttempts, " retry attempts ", "error:", err)
				break
			}

			time.Sleep(time.Second)

			log.Println("Retry attempt:", i+1, "Pending block not found for index:", sliceIndex, "error:", err)
		}
	}
	m.lock.Unlock()
	switch sliceIndex {
	case 0:
		m.pendingPrimeBlockCh <- receiptBlock
	case 1:
		m.pendingRegionBlockCh <- receiptBlock
	case 2:
		m.pendingZoneBlockCh <- receiptBlock
	}
}

// updateCombinedHeader performs the merged mining step of combining all headers from the slice of nodes
// being mined. This is then sent to the miner where a valid header is returned upon respective difficulties.
func (m *Manager) updateCombinedHeader(header *types.Header, i int) {
	m.lock.Lock()
	time := header.Time
	if time <= m.combinedHeader.Time {
		time = m.combinedHeader.Time
	}
	m.combinedHeader.ParentHash[i] = header.ParentHash[i]
	m.combinedHeader.UncleHash[i] = header.UncleHash[i]
	m.combinedHeader.Number[i] = header.Number[i]
	m.combinedHeader.Extra[i] = header.Extra[i]
	m.combinedHeader.BaseFee[i] = header.BaseFee[i]
	m.combinedHeader.GasLimit[i] = header.GasLimit[i]
	m.combinedHeader.GasUsed[i] = header.GasUsed[i]
	m.combinedHeader.TxHash[i] = header.TxHash[i]
	m.combinedHeader.ReceiptHash[i] = header.ReceiptHash[i]
	m.combinedHeader.Root[i] = header.Root[i]
	m.combinedHeader.Difficulty[i] = header.Difficulty[i]
	m.combinedHeader.NetworkDifficulty[i] = header.NetworkDifficulty[i]
	m.combinedHeader.Coinbase[i] = header.Coinbase[i]
	m.combinedHeader.Bloom[i] = header.Bloom[i]
	m.combinedHeader.Time = time
	m.combinedHeader.Location = m.location
	m.lock.Unlock()
}

// loopGlobalBlock takes in updates from the pending headers and blocks in order to update the miner.
// This sets the header information and puts the block data inside of pendingBlocks so that it can be retrieved
// upon a successful nonce being found.
func (m *Manager) loopGlobalBlock() error {
	for {
		select {
		case block := <-m.pendingPrimeBlockCh:
			header := block.Header()
			m.updateCombinedHeader(header, 0)
			m.pendingBlocks[0] = block
			header.Nonce = types.BlockNonce{}
			select {
			case m.updatedCh <- m.combinedHeader:
			default:
				log.Println("Sealing result is not read by miner", "mode", "fake", "sealhash")
			}
		case block := <-m.pendingRegionBlockCh:
			header := block.Header()
			m.updateCombinedHeader(header, 1)
			m.pendingBlocks[1] = block
			header.Nonce = types.BlockNonce{}
			select {
			case m.updatedCh <- m.combinedHeader:
			default:
				log.Println("Sealing result is not read by miner", "mode", "fake", "sealhash")
			}
		case block := <-m.pendingZoneBlockCh:
			header := block.Header()
			m.updateCombinedHeader(header, 2)
			m.pendingBlocks[2] = block
			header.Nonce = types.BlockNonce{}
			select {
			case m.updatedCh <- m.combinedHeader:
			default:
				log.Println("Sealing result is not read by miner", "mode", "fake", "sealhash")
			}
		}
	}
}

// check if the header is null. If so, don't start mining.
func (m *Manager) headerNullCheck() error {
	err := errors.New("header has nil value, cannot continue with mining")
	if m.combinedHeader.Number[0] == nil {
		log.Println("Waiting to retrieve Prime header information...")
		return err
	}
	if m.combinedHeader.Number[1] == nil {
		log.Println("Waiting to retrieve Region header information...")
		return err
	}
	if m.combinedHeader.Number[2] == nil {
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

			headerNull := m.headerNullCheck()
			if headerNull == nil {
				log.Println("Starting to mine block", header.Number, "@ location", m.location, "w/ difficulty", header.Difficulty)
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
					log.Println("Quai Miner - current hashes per second: ", hashRate)
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
		case bundle := <-m.resultCh:
			m.lock.Lock()
			header := bundle.Header

			if bundle.Context == 0 {
				log.Println("PRIME: ", header.Number, header.Hash())
			}

			if bundle.Context == 1 {
				log.Println("REGION:", header.Number, header.Hash())
			}

			if bundle.Context == 2 {
				log.Println("ZONE:  ", header.Number, header.Hash())
			}

			// Check to see that all nodes are running before sending blocks to them.
			if !m.allChainsOnline() {
				log.Println("At least one of the chains is not online at the moment")
				continue
			}

			// Check proper difficulty for which nodes to send block to
			// Notify blocks to put in cache before assembling new block on node
			if bundle.Context == 0 && header.Number[0] != nil {
				var wg sync.WaitGroup
				wg.Add(1)
				go m.SendClientsMinedExtBlock(0, []int{1, 2}, header, &wg)
				wg.Add(1)
				go m.SendClientsMinedExtBlock(1, []int{0, 2}, header, &wg)
				wg.Add(1)
				go m.SendClientsMinedExtBlock(2, []int{0, 1}, header, &wg)
				wg.Wait()
				wg.Add(1)
				go m.SendMinedBlock(2, header, &wg)
				wg.Add(1)
				go m.SendMinedBlock(1, header, &wg)
				wg.Add(1)
				go m.SendMinedBlock(0, header, &wg)
				wg.Wait()
			}

			// If Region difficulty send to Region
			if bundle.Context == 1 && header.Number[1] != nil {
				var wg sync.WaitGroup
				wg.Add(1)
				go m.SendClientsMinedExtBlock(1, []int{0, 2}, header, &wg)
				wg.Add(1)
				go m.SendClientsMinedExtBlock(2, []int{0, 1}, header, &wg)
				wg.Wait()
				wg.Add(1)
				go m.SendMinedBlock(2, header, &wg)
				wg.Add(1)
				go m.SendMinedBlock(1, header, &wg)
				wg.Wait()
			}

			// If Zone difficulty send to Zone
			if bundle.Context == 2 && header.Number[2] != nil {
				var wg sync.WaitGroup
				wg.Add(1)
				go m.SendClientsMinedExtBlock(2, []int{0, 1}, header, &wg)
				wg.Wait()
				wg.Add(1)
				go m.SendMinedBlock(2, header, &wg)
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
	for _, blockClient := range m.orderedBlockClients.regionClients {
		if !checkConnection(blockClient) {
			return false
		}
	}
	for i := range m.orderedBlockClients.zoneClients {
		for _, blockClient := range m.orderedBlockClients.zoneClients[i] {
			if !checkConnection(blockClient) {
				return false
			}
		}
	}
	return true
}

// SendClientsMinedExtBlock takes in the mined block and calls the pending blocks to send to the clients.
func (m *Manager) SendClientsMinedExtBlock(mined int, externalContexts []int, header *types.Header, wg *sync.WaitGroup) {
	receiptBlock := m.pendingBlocks[mined]
	if receiptBlock != nil {
		block := types.NewBlockWithHeader(header).WithBody(receiptBlock.Transactions(), receiptBlock.Uncles())
		m.SendClientsExtBlock(mined, externalContexts, block, receiptBlock)
	}
	defer wg.Done()
}

// SendClientsExtBlock takes in the mined block and the contexts of the mining slice to send the external block to.
// ex. mined 2, externalContexts []int{0, 1} will send the Zone external block to Prime and Region.
func (m *Manager) SendClientsExtBlock(mined int, externalContexts []int, block *types.Block, receiptBlock *types.ReceiptBlock) {
	// first send the external block to the mining chains
	blockLocation := block.Header().Location

	for i := 0; i < len(externalContexts); i++ {
		if externalContexts[i] == 0 && m.orderedBlockClients.primeAvailable {
			m.orderedBlockClients.primeClient.SendExternalBlock(context.Background(), block, receiptBlock.Receipts(), big.NewInt(int64(mined)))
		}
		if externalContexts[i] == 1 && m.orderedBlockClients.regionsAvailable[blockLocation[0]-1] {
			m.orderedBlockClients.regionClients[blockLocation[0]-1].SendExternalBlock(context.Background(), block, receiptBlock.Receipts(), big.NewInt(int64(mined)))
		}
		if externalContexts[i] == 2 && m.orderedBlockClients.zonesAvailable[blockLocation[0]-1][blockLocation[1]-1] {
			m.orderedBlockClients.zoneClients[blockLocation[0]-1][blockLocation[1]-1].SendExternalBlock(context.Background(), block, receiptBlock.Receipts(), big.NewInt(int64(mined)))
		}
	}
	// sending the external blocks to chains other than the mining chains
	for i, blockClient := range m.orderedBlockClients.regionClients {
		miningRegion := int(blockLocation[0])-1 == i
		if !miningRegion {
			blockClient.SendExternalBlock(context.Background(), block, receiptBlock.Receipts(), big.NewInt(int64(mined)))
		}
	}

	for i := range m.orderedBlockClients.zoneClients {
		for j, blockClient := range m.orderedBlockClients.zoneClients[i] {
			miningZone := int(blockLocation[0])-1 == i && int(blockLocation[1])-1 == j
			if !miningZone {
				blockClient.SendExternalBlock(context.Background(), block, receiptBlock.Receipts(), big.NewInt(int64(mined)))
			}
		}
	}

}

// SendMinedBlock sends the mined block to its mining client with the transactions, uncles, and receipts.
func (m *Manager) SendMinedBlock(mined int, header *types.Header, wg *sync.WaitGroup) {
	receiptBlock := m.pendingBlocks[mined]
	block := types.NewBlockWithHeader(receiptBlock.Header()).WithBody(receiptBlock.Transactions(), receiptBlock.Uncles())
	if block != nil {
		sealed := block.WithSeal(header)
		if mined == 0 {
			m.orderedBlockClients.primeClient.SendMinedBlock(context.Background(), sealed, true, true)
		}
		if mined == 1 {
			m.orderedBlockClients.regionClients[m.location[0]-1].SendMinedBlock(context.Background(), sealed, true, true)
		}
		if mined == 2 {
			m.orderedBlockClients.zoneClients[m.location[0]-1][m.location[1]-1].SendMinedBlock(context.Background(), sealed, true, true)
		}
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

// Examines the Quai Network to find the Region-Zone location with lowest difficulty.
func findBestLocation(clients orderedBlockClients) []byte {
	lowestRegion := big.NewInt(math.MaxInt) // integer for holding lowest Region difficulty
	lowestZone := big.NewInt(math.MaxInt)   // integer for holding lowest Zone difficulty
	var regionLocation int                  // remember to return location as []byte with Zone1-1 = [1,1]
	var zoneLocation int

	// first find the Region chain with lowest difficulty
	for i, client := range clients.regionClients {
		latestHeader, err := client.HeaderByNumber(context.Background(), nil)
		if err != nil {
			log.Println("Error: connection lost during request")
			log.Println(err)
		} else {
			difficulty := latestHeader.Difficulty[1]
			if difficulty.Cmp(lowestRegion) == -1 {
				regionLocation = i + 1
				lowestRegion = difficulty
			}
			fmt.Println("region ", i+1, " difficulty ", difficulty)
		}
	}
	// next find Zone chain inside Region with lowest difficulty
	for i, client := range clients.zoneClients[regionLocation-1] {
		latestHeader, err := client.HeaderByNumber(context.Background(), nil)
		if err != nil {
			log.Println("Error: connect lost during request")
			log.Println(err)
		} else {
			difficulty := latestHeader.Difficulty[2]
			if difficulty.Cmp(lowestZone) == -1 {
				zoneLocation = i + 1
				lowestZone = difficulty
			}
			fmt.Println("zone ", i+1, " difficulty ", difficulty)
		}
	}

	// print location selected
	fmt.Println("Region location selected: ", regionLocation)
	fmt.Println("Zone location selected: ", zoneLocation)
	regionBytes := make([]byte, 8)
	zoneBytes := make([]byte, 8)
	binary.LittleEndian.PutUint64(regionBytes, uint64(regionLocation))
	binary.LittleEndian.PutUint64(zoneBytes, uint64(zoneLocation))
	// return location to config
	return []byte{regionBytes[0], zoneBytes[0]}
}

// Checks for best location to mine every 10 minutes;
// if better location is found it will initiate the change to the config.
func (m *Manager) checkBestLocation(timer int) {
	ticker := time.NewTicker(time.Duration(timer) * time.Minute)
	go func() {
		for {
			select {
			case <-exit:
				ticker.Stop()
				return
			case <-ticker.C:
				newLocation := findBestLocation(m.orderedBlockClients)
				// check if location has changed, and if true, update mining processes
				if !bytes.Equal(newLocation, m.location) {
					m.doneCh <- true // channel to make current processes stop
					m.location = newLocation
					m.doneCh <- false // set back to false to let new mining processes start
					m.subscribeAllPendingBlocks()
					m.fetchAllPendingBlocks()
				}
			}
		}
	}()
}

// Bundle of goroutines that need to be stopped and restarted if/when location updates.
func (m *Manager) subscribeAllPendingBlocks() {
	// subscribing to the pending blocks
	if m.orderedBlockClients.primeAvailable && checkConnection(m.orderedBlockClients.primeClient) {
		go m.subscribePendingHeader(m.orderedBlockClients.primeClient, 0)
	}
	if m.orderedBlockClients.regionsAvailable[m.location[0]-1] && checkConnection(m.orderedBlockClients.regionClients[m.location[0]-1]) {
		go m.subscribePendingHeader(m.orderedBlockClients.regionClients[m.location[0]-1], 1)
	}
	if m.orderedBlockClients.zonesAvailable[m.location[0]-1][m.location[1]-1] && checkConnection(m.orderedBlockClients.zoneClients[m.location[0]-1][m.location[1]-1]) {
		go m.subscribePendingHeader(m.orderedBlockClients.zoneClients[m.location[0]-1][m.location[1]-1], 2)
	}
}

// Bundle of goroutines that need to be stopped and restarted if/when location updates.
func (m *Manager) fetchAllPendingBlocks() {
	if m.orderedBlockClients.primeAvailable && checkConnection(m.orderedBlockClients.primeClient) {
		go m.fetchPendingBlocks(m.orderedBlockClients.primeClient, 0)
	}
	if m.orderedBlockClients.regionsAvailable[m.location[0]-1] && checkConnection(m.orderedBlockClients.regionClients[m.location[0]-1]) {
		go m.fetchPendingBlocks(m.orderedBlockClients.regionClients[m.location[0]-1], 1)
	}
	if m.orderedBlockClients.zonesAvailable[m.location[0]-1][m.location[1]-1] && checkConnection(m.orderedBlockClients.zoneClients[m.location[0]-1][m.location[1]-1]) {
		go m.fetchPendingBlocks(m.orderedBlockClients.zoneClients[m.location[0]-1][m.location[1]-1], 2)
	}
}
