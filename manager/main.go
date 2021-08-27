package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/ethash"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/ethclient"
	"github.com/ethereum/go-ethereum/params"
	"github.com/gorilla/websocket"
	lru "github.com/hashicorp/golang-lru"
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

	clientSlice      []*ethclient.Client
	availableClients []bool
	combinedHeader   *types.Header
	pendingBlocks    []*types.Block // Current pending blocks of the manager
	lock             sync.Mutex
	conns            []*wsConn // Currently live websocket connections

	pendingPrimeBlockCh  chan *types.Block
	pendingRegionBlockCh chan *types.Block
	pendingZoneBlockCh   chan *types.Block

	updatedCh chan *types.Header
	resultCh  chan *types.HeaderBundle
	startCh   chan struct{}
	exitCh    chan struct{}

	BlockCache [][]*lru.Cache // Cache for the most recent entire blocks
}

// wsConn wraps a websocket connection with a write mutex as the underlying
// websocket library does not synchronize access to the stream.
type wsConn struct {
	conn  *websocket.Conn
	wlock sync.Mutex
}

func main() {
	config, err := util.LoadConfig("..")
	if err != nil {
		log.Fatal("cannot load config:", err)
	}

	clientSlice := make([]*ethclient.Client, 3)
	available := make([]bool, 3)

	if config.PrimeMiningNode != "" {
		clientSlice[0], err = ethclient.Dial(config.PrimeMiningNode)
		if err != nil {
			fmt.Println("Error connecting to Prime mining node")
		} else {
			available[0] = true
		}
	}

	if config.RegionMiningNode != "" {
		clientSlice[1], err = ethclient.Dial(config.RegionMiningNode)
		if err != nil {
			fmt.Println("Error connecting to Region mining node")
		} else {
			available[1] = true
		}
	}

	if config.ZoneMiningNode != "" {
		clientSlice[2], err = ethclient.Dial(config.ZoneMiningNode)
		if err != nil {
			fmt.Println("Error connecting to Zone mining node")
		} else {
			available[2] = true
		}
	}

	header := &types.Header{
		ParentHash:  make([]common.Hash, 3),
		Number:      make([]*big.Int, 3),
		Extra:       make([][]byte, 3),
		Time:        uint64(0),
		BaseFee:     make([]*big.Int, 3),
		GasLimit:    make([]uint64, 3),
		Coinbase:    make([]common.Address, 3),
		Difficulty:  make([]*big.Int, 3),
		Root:        make([]common.Hash, 3),
		TxHash:      make([]common.Hash, 3),
		UncleHash:   make([]common.Hash, 3),
		ReceiptHash: make([]common.Hash, 3),
		GasUsed:     make([]uint64, 3),
		Bloom:       make([]types.Bloom, 3),
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
		clientSlice:          clientSlice,
		availableClients:     available,
		combinedHeader:       header,
		pendingBlocks:        make([]*types.Block, 3),
		pendingPrimeBlockCh:  make(chan *types.Block, resultQueueSize),
		pendingRegionBlockCh: make(chan *types.Block, resultQueueSize),
		pendingZoneBlockCh:   make(chan *types.Block, resultQueueSize),
		resultCh:             make(chan *types.HeaderBundle, resultQueueSize),
		updatedCh:            make(chan *types.Header, resultQueueSize),
		exitCh:               make(chan struct{}),
		startCh:              make(chan struct{}, 1),
	}

	for i := 0; i < len(m.availableClients); i++ {
		if m.availableClients[i] {
			go m.subscribePendingHeader(i)
		}
	}

	go m.resultLoop()

	go m.miningLoop()

	go m.WatchHashRate()

	go m.loopGlobalBlock()

	for i := 0; i < len(m.availableClients); i++ {
		if m.availableClients[i] {
			m.fetchPendingBlocks(i)
		}
	}
	<-exit
}

func (m *Manager) subscribePendingHeader(sliceIndex int) {
	// Wait for chain events and push them to clients
	header := make(chan *types.Header)
	sub, err := m.clientSlice[sliceIndex].SubscribePendingBlock(context.Background(), header)
	fmt.Println("Subscribed to pending block headers")
	if err != nil {
		log.Fatal("Failed to subscribe to pending block events", err)
	}
	defer sub.Unsubscribe()

	// Wait for various events and assing to the appropriate background threads
	for {
		select {
		case h := <-header:
			// New head arrived, send if for state update if there's none running
			fmt.Println("New pending block", "number", h.Number[sliceIndex])
			m.fetchPendingBlocks(sliceIndex)
		}
	}
}

func (m *Manager) fetchPendingBlocks(sliceIndex int) {
	block, err := m.clientSlice[sliceIndex].GetPendingBlock(context.Background())
	if err != nil {
		log.Fatal("Pending block not found: ", err)
	}
	switch sliceIndex {
	case 0:
		fmt.Println("Sending prime block to channel")
		m.pendingPrimeBlockCh <- block
	case 1:
		fmt.Println("Sending region block to channel")
		m.pendingRegionBlockCh <- block
	case 2:
		fmt.Println("Sending zone block to channel")
		m.pendingZoneBlockCh <- block
	}
}

func (m *Manager) updateCombinedHeader(header *types.Header, i int) {
	m.lock.Lock()
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
	m.combinedHeader.Coinbase[i] = header.Coinbase[i]
	m.combinedHeader.Bloom[i] = header.Bloom[i]
	m.combinedHeader.Time = header.Time
	m.lock.Unlock()

}

func (m *Manager) loopGlobalBlock() error {
	for {
		select {
		case block := <-m.pendingPrimeBlockCh:
			header := block.Header()
			m.updateCombinedHeader(header, 0)
			fmt.Println("Prime combined header info", m.combinedHeader.Number)
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
			fmt.Println("Region combined header info", m.combinedHeader.Number)
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
			fmt.Println("Zone combined header info", m.combinedHeader.Number)
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
			fmt.Println("Mining block with numbers", header.Number)

			// Interrupt previous sealing operation
			interrupt()
			stopCh = make(chan struct{})
			if err := m.engine.MergedMineSeal(header, m.resultCh, stopCh); err != nil {
				fmt.Println("Block sealing failed", "err", err)
			}
		}
	}
}

func (m *Manager) WatchHashRate() {
	ticker := time.NewTicker(10 * time.Second)
	go func() {
		for {
			select {
			case <-ticker.C:
				fmt.Println("Current Hashrate", m.engine.Hashrate())
			}
		}
	}()
}

func (m *Manager) resultLoop() error {
	for {
		select {
		case bundle := <-m.resultCh:

			header := bundle.Header

			if bundle.Context == 0 {
				fmt.Println("Found a Prime block!")
			}

			if bundle.Context == 1 {
				fmt.Println("Found a Region block!")
			}

			if bundle.Context == 2 {
				fmt.Println("Found a Zone block!")
			}

			// Check proper difficulty for which nodes to send block to
			// If Prime difficulty send to Prime
			if bundle.Context == 0 && header.Number[1] != nil && m.availableClients[0] {
				block := m.pendingBlocks[0]
				if block != nil {
					sealed := block.WithSeal(header)
					fmt.Println("Sending block to Prime", header.Number)
					m.clientSlice[0].SendMinedBlock(context.Background(), sealed, true, true)
					m.clientSlice[1].SendExternalBlock(context.Background(), sealed, big.NewInt(0))
					m.clientSlice[2].SendExternalBlock(context.Background(), sealed, big.NewInt(0))
				}
			}

			// If Region difficulty send to Region
			if bundle.Context <= 1 && header.Number[1] != nil && m.availableClients[1] {
				block := m.pendingBlocks[2]
				if block != nil {
					sealed := block.WithSeal(header)
					fmt.Println("Sending block to Region", header.Number)
					m.clientSlice[1].SendMinedBlock(context.Background(), sealed, true, true)
					m.clientSlice[0].SendExternalBlock(context.Background(), sealed, big.NewInt(1))
					m.clientSlice[2].SendExternalBlock(context.Background(), sealed, big.NewInt(1))
				}
			}

			// If Zone difficulty send to Zone
			if bundle.Context <= 2 && header.Number[2] != nil && m.availableClients[2] {
				block := m.pendingBlocks[2]
				if block != nil {
					sealed := block.WithSeal(header)
					fmt.Println("Sending block to Zone", header.Number)
					m.clientSlice[2].SendMinedBlock(context.Background(), sealed, true, true)
					m.clientSlice[0].SendExternalBlock(context.Background(), sealed, big.NewInt(2))
					m.clientSlice[1].SendExternalBlock(context.Background(), sealed, big.NewInt(2))
				}
			}
		}
	}
}
