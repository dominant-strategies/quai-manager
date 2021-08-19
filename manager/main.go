package main

import (
	"context"
	"fmt"
	"log"
	"math/big"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/common"
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

	ContextDepth = 1
)

var exit = make(chan bool)

type Manager struct {
	config *params.ChainConfig // Chain configurations for signing

	clientSlice      []*ethclient.Client
	availableClients []bool
	combinedHeader   *types.Header
	pendingBlocks    []*types.Block // Current pending blocks of the manager
	lock             []sync.RWMutex
	conns            []*wsConn // Currently live websocket connections

	pendingPrimeBlockCh  chan *types.Block
	pendingRegionBlockCh chan *types.Block
	pendingZoneBlockCh   chan *types.Block

	resultCh chan *types.Block
	startCh  chan struct{}
	exitCh   chan struct{}

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
			fmt.Println("Error connecting to Prime mining node")
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
		ReceiptHash: make([]common.Hash, 3),
		GasUsed:     make([]uint64, 3),
		Bloom:       make([]types.Bloom, 3),
	}

	m := &Manager{
		clientSlice:          clientSlice,
		availableClients:     available,
		combinedHeader:       header,
		pendingBlocks:        make([]*types.Block, 3),
		pendingPrimeBlockCh:  make(chan *types.Block, resultQueueSize),
		pendingRegionBlockCh: make(chan *types.Block, resultQueueSize),
		pendingZoneBlockCh:   make(chan *types.Block, resultQueueSize),
		resultCh:             make(chan *types.Block, resultQueueSize),
		exitCh:               make(chan struct{}),
		startCh:              make(chan struct{}, 1),
		lock:                 make([]sync.RWMutex, 3),
	}

	for i := 0; i < len(m.availableClients); i++ {
		if m.availableClients[i] {
			go m.subscribePendingHeader(i)
		}
	}

	go m.resultLoop()

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
	fmt.Println("Subscribed to pending blocks")
	if err != nil {
		log.Fatal("Failed to subscribe to pending block events", err)
	}
	defer sub.Unsubscribe()

	// Wait for various events and assing to the appropriate background threads
	for {
		select {
		case h := <-header:
			// New head arrived, send if for state update if there's none running
			fmt.Println("New pending block", "number", h.Number)
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
		m.pendingPrimeBlockCh <- block
	case 1:
		m.pendingRegionBlockCh <- block
	case 2:
		m.pendingZoneBlockCh <- block
	}
}

func (m *Manager) updateCombinedHeader(header *types.Header, i int) {
	m.combinedHeader.ParentHash[i] = header.ParentHash[i]
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
}

func (m *Manager) loopGlobalBlock() error {
	for {
		select {
		case block := <-m.pendingZoneBlockCh:
			time.Sleep(3 * time.Second)
			header := block.Header()
			m.updateCombinedHeader(header, 2)
			m.pendingBlocks[2] = block
			header.Nonce, header.MixDigest = types.BlockNonce{}, []common.Hash{common.Hash{}, common.Hash{}, common.Hash{}}
			select {
			case m.resultCh <- block.WithSeal(header):
			default:
				fmt.Println("Sealing result is not read by miner", "mode", "fake", "sealhash")
			}
		}
	}
}

func (m *Manager) resultLoop() error {
	for {
		select {
		case block := <-m.resultCh:
			fmt.Println("Sending Mined Block", block.Header().Number)
			// Check proper difficulty for which nodes to send block to
			for i := 0; i < len(m.availableClients); i++ {
				if m.availableClients[i] {
					m.clientSlice[i].SendMinedBlock(context.Background(), block, true, true)
				}
			}
		}
	}
}
