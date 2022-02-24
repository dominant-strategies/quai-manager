package main

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log"
	"math/big"
	"math/rand"
	"os"
	"strconv"
	"sync"
	"time"

	lru "github.com/hashicorp/golang-lru"
	"github.com/spruce-solutions/go-quai/common"
	"github.com/spruce-solutions/go-quai/common/hexutil"
	"github.com/spruce-solutions/go-quai/consensus/ethash"
	"github.com/spruce-solutions/go-quai/core"
	"github.com/spruce-solutions/go-quai/core/types"
	"github.com/spruce-solutions/go-quai/crypto"
	"github.com/spruce-solutions/go-quai/ethclient"
	"github.com/spruce-solutions/go-quai/params"
	"github.com/spruce-solutions/quai-manager/manager/util"
)

const (
	// resultQueueSize is the size of channel listening to sealing result.
	resultQueueSize = 10
)

var exit = make(chan bool)

type Manager struct {
	config *params.ChainConfig // Chain configurations for signing
	engine *ethash.Ethash

	orderedBlockClients []orderedBlockClient // will hold all chain URLs and settings in order from prime to zone-3-3
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

	BlockCache [][]*lru.Cache // Cache for the most recent entire blocks
}

// Block struct to hold all Client fields.
type orderedBlockClient struct {
	chainAvailable bool
	chainClient    *ethclient.Client
	chainContext   int
}

func main() {
	config, err := util.LoadConfig("..")
	if err != nil {
		log.Fatal("cannot load config:", err)
	}

	if len(os.Args) > 3 {
		location := os.Args[1:3]

		mine, _ := strconv.Atoi(os.Args[3:][0])

		if len(location) == 0 {
			log.Fatal("Please mention the location where you want to mine")
		}

		if len(location) == 1 {
			log.Fatal("You are missing either the region or zone location")
		}

		if len(location) > 2 {
			log.Fatal("Only specify 2 values for the location")
		}

		// converting region and zone location values from string to integer
		regionLoc, _ := strconv.Atoi(location[0])
		zoneLoc, _ := strconv.Atoi(location[1])

		// converting the region and zone integer values to bytes
		RegionLocArr := make([]byte, 8)
		ZoneLocArr := make([]byte, 8)
		binary.LittleEndian.PutUint64(RegionLocArr, uint64(regionLoc))
		binary.LittleEndian.PutUint64(ZoneLocArr, uint64(zoneLoc))

		config.Location = []byte{RegionLocArr[0], ZoneLocArr[0]}
		config.Mine = mine == 1
	}
	// Get URLs for all chains and set mining bools; if true then mine
	allClients := getMiningClients(config)

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

	sharedConfig := ethash.Config{
		PowMode:       ethash.ModeNormal,
		CachesInMem:   3,
		DatasetsInMem: 1,
	}

	ethashEngine := ethash.New(sharedConfig, nil, false)
	ethashEngine.SetThreads(4)
	m := &Manager{
		engine:               ethashEngine,
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
		location:             config.Location,
	}

	go m.subscribeNewHead()

	go m.subscribeReOrg()

	if config.Mine == true {
		fmt.Println("Starting manager in location ", config.Location)
		for _, blockClient := range m.orderedBlockClients {
			if blockClient.chainAvailable == true {
				go m.subscribePendingHeader(blockClient)
			}
		}
		// next point of work
		go m.resultLoop()

		go m.miningLoop()

		go m.SubmitHashRate()

		go m.loopGlobalBlock()

		for i := 0; i < len(m.miningClients); i++ {
			if m.miningAvailable[i] {
				m.fetchPendingBlocks(i)
			}
		}
	}
	<-exit
}

// getMiningClients takes in a config and retrieves the Prime, Region, and Zone client
// that is used for mining in a slice.
func getMiningClients(config util.Config) []orderedBlockClient {
	allClients := []orderedBlockClient{}

	// add Prime to orderedBlockClient array at [0]
	if config.PrimeURL != "" {
		primeBlockClient := orderedBlockClient{}
		primeClient, err := ethclient.Dial(config.PrimeURL)
		if err != nil {
			fmt.Println("Error connecting to Prime mining node")
		} else {
			primeBlockClient.chainAvailable = true
			primeBlockClient.chainClient = primeClient
			primeBlockClient.chainContext = 0
			allClients = append(allClients, primeBlockClient)
		}
	}

	// loop to add Regions to orderedBlockClient
	// remember to set true value for Region to be mined
	for i, URL := range config.RegionURLs {
		regionURL := URL
		if regionURL != "" {
			regionBlockClient := orderedBlockClient{}
			regionClient, err := ethclient.Dial(regionURL)
			if err != nil {
				fmt.Println("Error connecting to Region mining node %d in location %b", URL, i)
			} else {
				if i == int(config.Location[0]) {
					regionBlockClient.chainAvailable = true
				} else {
					regionBlockClient.chainAvailable = false
				}
				regionBlockClient.chainClient = regionClient
				regionBlockClient.chainContext = 1
				allClients = append(allClients, regionBlockClient)
			}
		}
	}

	// loop to add Zones to orderedBlockClient
	// remember ZoneURLS is a 2D array
	for i, zonesURLs := range config.ZoneURLs {
		for j, zoneURL := range zonesURLs {
			if zoneURL != "" {
				zoneBlockClient := orderedBlockClient{}
				zoneClient, err := ethclient.Dial(zoneURL)
				if err != nil {
					fmt.Println("Error connecting to Zone mining node")
				} else {
					if i == int(config.Location[0]) && j == int(config.Location[1]) {
						zoneBlockClient.chainAvailable = true
					} else {
						zoneBlockClient.chainAvailable = false
					}
					zoneBlockClient.chainClient = zoneClient
					zoneBlockClient.chainContext = 2
					allClients = append(allClients, zoneBlockClient)
				}
			}
		}
	}
	return allClients
}

// subscribePendingHeader subscribes to the head of the mining nodes in order to pass
// the most up to date block to the miner within the manager.
func (m *Manager) subscribePendingHeader(client orderedBlockClient) {
	// check the status of the sync
	checkSync, err := client.chainClient.SyncProgress(context.Background())
	sliceIndex := client.chainContext

	if err != nil {
		switch sliceIndex {
		case 0:
			fmt.Println("Error occured while synching to Prime")
		case 1:
			fmt.Println("Error occured while synching to Region")
		case 2:
			fmt.Println("Error occured while synching to Zone")
		}
	}

	// wait until sync is nil to continue
	for checkSync != nil && err == nil {
		checkSync, err = client.chainClient.SyncProgress(context.Background())
	}

	// subscribe to the pending block only if not synching
	if checkSync == nil && err == nil {
		// Wait for chain events and push them to clients
		header := make(chan *types.Header)
		sub, err := client.chainClient.SubscribePendingBlock(context.Background(), header)
		if err != nil {
			log.Fatal("Failed to subscribe to pending block events", err)
		}
		defer sub.Unsubscribe()

		// Wait for various events and assing to the appropriate background threads
		for {
			select {
			case <-header:
				// New head arrived, send if for state update if there's none running
				m.fetchPendingBlocks(client)
			}
		}
	}
}

// subscribeNewHead passes new head blocks as external blocks to lower level chains.
func (m *Manager) subscribeNewHead() {

	prime := "prime"
	regions := [3]string{"region-1", "region-2", "region-3"}

	// subscribe to the prime client
	m.subscribeNewHeadClient(m.orderedBlockClients[0].chainClient, prime, 0)
	// subscribe to the region client on the mining location
	for _, blockClient := range m.orderedBlockClients[1:types.ContextDepth] {
		if blockClient.chainAvailable == true {
			m.subscribeNewHeadClient(blockClient.chainClient, regions[m.location[0]-1], 1)
			break
		}
	}

	// subscribe to external region contexts
	for _, blockClient := range m.orderedBlockClients[1:types.ContextDepth] {
		if blockClient.chainAvailable == false {
			m.subscribeNewHeadClient(blockClient.chainClient, regions[m.location[0]-1], 1)
		}
	}
}

func (m *Manager) subscribeNewHeadClient(client *ethclient.Client, location string, difficultyContext int) {
	newHeadChannel := make(chan *types.Header, 1)
	retryAttempts := 5
	sub, err := client.SubscribeNewHead(context.Background(), newHeadChannel)
	if err != nil {
		log.Fatal("Failed to subscribe to the reorg notifications in ", location, err)
	}
	defer sub.Unsubscribe()

	for {
		select {
		case newHead := <-newHeadChannel:
			// get the block and receipt block
			fmt.Println("Retrieved new head", "hash", newHead.Hash())
			block, err := client.BlockByHash(context.Background(), newHead.Hash())
			if err != nil {
				fmt.Println("Failed to retrieve block for hash", "hash ", newHead.Hash())

				for i := 0; ; i++ {
					block, err = client.BlockByHash(context.Background(), newHead.Hash())
					if err == nil {
						break
					}

					if i >= retryAttempts {
						fmt.Println("Failed to retrieve block for hash ", "hash ", newHead.Hash(), " even after ", retryAttempts, " retry attempts ")
						return
					}

					time.Sleep(12 * time.Second)

					fmt.Println("Retry attempt:", i+1, "Failed to retrieve block for hash ", "hash", newHead.Hash())
				}
			}

			receiptBlock, receiptErr := client.GetBlockReceipts(context.Background(), newHead.Hash())
			if receiptErr != nil {
				fmt.Println("Failed to retrieve receipts for block", "hash", newHead.Hash())

				for i := 0; ; i++ {
					receiptBlock, receiptErr = client.GetBlockReceipts(context.Background(), newHead.Hash())
					if receiptErr == nil {
						break
					}

					if i >= retryAttempts {
						log.Fatal("Failed to retrieve receipts for block", "hash", newHead.Hash(), " even after ", retryAttempts, " retry attempts ")
					}

					time.Sleep(12 * time.Second)

					fmt.Println("Retry attempt:", i+1, "Failed to retrieve receipts for block", "hash", newHead.Hash())
				}
			}

			if difficultyContext == 0 {
				m.SendClientsExtBlock(difficultyContext, []int{1, 2}, block, receiptBlock)
			} else if difficultyContext == 1 {
				m.SendClientsExtBlock(difficultyContext, []int{2}, block, receiptBlock)
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
	m.subscribeReOrgClients(m.orderedBlockClients[0].chainClient, prime, 0)
	// for-if statement to loop over Region allClients and select available Region
	for i := 1; i < len(m.orderedBlockClients[1:3]); i++ {
		if m.orderedBlockClients[i].chainAvailable == true {
			m.subscribeReOrgClients(m.orderedBlockClients[i].chainClient, regions[m.location[0]-1], 1)
			break
		}
	}

	//subscribe to the regions from external contexts
	for i := 1; i < len(m.orderedBlockClients[1:3]); i++ {
		if m.orderedBlockClients[i].chainAvailable == false {
			m.subscribeReOrgClients(m.orderedBlockClients[i].chainClient, regions[m.location[0]-1], 1)
		}
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
			heavier := compareReorgDifficulty(reOrgData.ReOrgHeader, reOrgData.OldChainHeaders, reOrgData.NewChainHeaders, difficultyContext)
			if heavier {
				m.sendReOrgHeader(reOrgData.NewChainHeaders[len(reOrgData.NewChainHeaders)-2], location)
			}
		}
	}
}

// sendReOrgHeader sends the reorg header to the respective region and zone clients
func (m *Manager) sendReOrgHeader(header *types.Header, location string) {
	if location == "prime" {
		// if the reorg event takes palce in prime then have to send the header to all
		// the chains except for prime
		for _, blockClient := range m.orderedBlockClients[1:] { // start at 1 to skip Prime
			blockClient.chainClient.SendReOrgData(context.Background(), header) // all clients pass thru regardless if mining
		}
	} else { // regions
		// only subscribe to the zones
		reorgLocation := getRegionIndex(location)
		if reorgLocation == int(m.location[0]) {
			// send to the zone chain in the mining client and send to two other chains in the external clients
			for _, blockClient := range m.orderedBlockClients[4:] {
				blockClient.chainClient.SendReOrgData(context.Background(), header)
			}
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

// fetchPendingBlocks gets the latest block when we have received a new pending header. This will get the receipts,
// transactions, and uncles to be stored during mining.
func (m *Manager) fetchPendingBlocks(client orderedBlockClient) {
	retryAttempts := 5
	var receiptBlock *types.ReceiptBlock
	var err error
	sliceIndex := client.chainContext

	m.lock.Lock()
	receiptBlock, err = client.chainClient.GetPendingBlock(context.Background())

	// check for stale headers and refetch the latest header
	if receiptBlock.Header().Number[sliceIndex] == m.combinedHeader.Number[sliceIndex] && err == nil {
		switch sliceIndex {
		case 0:
			fmt.Println("Expected header numbers don't match for Prime at block height", receiptBlock.Header().Number[0])
			fmt.Println("Retrying and attempting to refetch the latest header for Prime")
		case 1:
			fmt.Println("Expected header numbers don't match for Region at block height", receiptBlock.Header().Number[1])
			fmt.Println("Retrying and attempting to refetch the latest header for Region")
		case 2:
			fmt.Println("Expected header numbers don't match for Zone at block height", receiptBlock.Header().Number[2])
			fmt.Println("Retrying and attempting to refetch the latest header for Zone")
		}
		receiptBlock, err = client.chainClient.GetPendingBlock(context.Background())
	}

	// retrying for 5 times if pending block not found
	if err != nil {
		fmt.Println("Pending block not found for index:", sliceIndex, "error:", err)

		for i := 0; ; i++ {
			receiptBlock, err = client.chainClient.GetPendingBlock(context.Background())
			if err == nil {
				break
			}

			if i >= retryAttempts {
				fmt.Println("Pending block was never found for index:", sliceIndex, " even after ", retryAttempts, " retry attempts ", "error:", err)
				return
			}

			time.Sleep(time.Second)

			fmt.Println("Retry attempt:", i+1, "Pending block not found for index:", sliceIndex, "error:", err)
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
			header.Nonce, header.MixDigest = types.BlockNonce{}, common.Hash{}
			select {
			case m.updatedCh <- m.combinedHeader:
			default:
				fmt.Println("Sealing result is not read by miner", "mode", "fake", "sealhash")
			}
		case block := <-m.pendingRegionBlockCh:
			header := block.Header()
			m.updateCombinedHeader(header, 1)
			m.pendingBlocks[1] = block
			header.Nonce, header.MixDigest = types.BlockNonce{}, common.Hash{}
			select {
			case m.updatedCh <- m.combinedHeader:
			default:
				fmt.Println("Sealing result is not read by miner", "mode", "fake", "sealhash")
			}
		case block := <-m.pendingZoneBlockCh:
			header := block.Header()
			m.updateCombinedHeader(header, 2)
			m.pendingBlocks[2] = block
			header.Nonce, header.MixDigest = types.BlockNonce{}, common.Hash{}
			select {
			case m.updatedCh <- m.combinedHeader:
			default:
				fmt.Println("Sealing result is not read by miner", "mode", "fake", "sealhash")
			}
		}
	}
}

// check if the header is null. If so, don't start mining.
func (m *Manager) headerNullCheck() error {
	err := errors.New("header has nil value, cannot continue with mining")
	if m.combinedHeader.Number[0] == nil {
		fmt.Println("Header for the Prime is nil, waiting for the Prime header to start mining")
		return err
	}
	if m.combinedHeader.Number[1] == nil {
		fmt.Println("Header for the Region is nil, waiting for the Region header to start mining")
		return err
	}
	if m.combinedHeader.Number[2] == nil {
		fmt.Println("Header for the Zone is nil, waiting for the Zone header to start mining")
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
				if err := m.engine.MergedMineSeal(header, m.resultCh, stopCh); err != nil {
					fmt.Println("Block sealing failed", "err", err)
				}
			}
		}
	}
}

// WatchHashRate is a simple method to watch the hashrate of our miner and log the output.
func (m *Manager) SubmitHashRate() {
	ticker := time.NewTicker(10 * time.Second)

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
	for _, externalContext := range externalContexts {
		for _, blockClient := range m.orderedBlockClients {
			if blockClient.chainAvailable == true && blockClient.chainContext == externalContext {
				blockClient.chainClient.SendExternalBlock(context.Background(), block, receiptBlock.Receipts(), big.NewInt(int64(mined)))
			}
		}
	}
}

// SendMinedBlock sends the mined block to its mining client with the transactions, uncles, and receipts.
func (m *Manager) SendMinedBlock(mined int, header *types.Header, wg *sync.WaitGroup) {
	receiptBlock := m.pendingBlocks[mined]
	block := types.NewBlockWithHeader(receiptBlock.Header()).WithBody(receiptBlock.Transactions(), receiptBlock.Uncles())
	if block != nil {
		for _, blockClient := range m.orderedBlockClients {
			if blockClient.chainAvailable == true && blockClient.chainContext == mined {
				sealed := block.WithSeal(header)
				blockClient.chainClient.SendMinedBlock(context.Background(), sealed, true, true)
			}
		}
	}
	defer wg.Done()
}
