package main

import (
	"context"
	"fmt"
	"log"
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

type portMap struct {
}

type latest struct {
	LatestPrime  common.Hash `json:"latestprime"`
	LatestRegion common.Hash `json:"latestregion"`
	LatestZone   common.Hash `json:"latestzone"`
}

type Manager struct {
	config *params.ChainConfig // Chain configurations for signing

	ClientSlice []*ethclient.Client

	headers []*types.Header // Current head headers of the manager
	lock    []sync.RWMutex
	conns   []*wsConn // Currently live websocket connections

	MiningAddr []common.Address
	BlockCache [][]*lru.Cache // Cache for the most recent entire blocks
}

// wsConn wraps a websocket connection with a write mutex as the underlying
// websocket library does not synchronize access to the stream.
type wsConn struct {
	conn  *websocket.Conn
	wlock sync.Mutex
}

var ContextDepth = 2
var exit = make(chan bool)

func main() {
	config, err := util.LoadConfig("..")
	if err != nil {
		log.Fatal("cannot load config:", err)
	}

	clientPrime, err := ethclient.Dial(config.MiningSliceUrls[0])
	clientRegion, err := ethclient.Dial(config.MiningSliceUrls[1])
	clientZone, err := ethclient.Dial(config.MiningSliceUrls[2])

	clientSlice := []*ethclient.Client{clientPrime, clientRegion, clientZone}
	m := &Manager{
		ClientSlice: clientSlice,
		headers:     make([]*types.Header, 3),
	}

	m.fetchLatestHeads()

	// Loop on receiving blocks
	go m.subscribeHead(0)
	go m.subscribeHead(1)
	// go m.loop(2)
	<-exit

	// Loop on mining blocks
}

func (m *Manager) fetchLatestHeads() {
	for i := 0; i < ContextDepth; i++ {
		header, err := m.ClientSlice[i].HeaderByNumber(context.Background(), nil)
		if err != nil {
			log.Fatal("Intitial header not found: ", err)
		}
		m.headers[i] = header
		fmt.Println("Found header at index", i, "with hash", header.Hash())
	}
}

// loop keeps waiting for interesting events and pushes them out to connected
// websockets.
func (m *Manager) subscribeHead(sliceIndex int) {
	// Wait for chain events and push them to clients
	heads := make(chan *types.Header, 16)
	sub, err := m.ClientSlice[sliceIndex].SubscribeNewHead(context.Background(), heads)
	fmt.Println("Subscribed to latest head")
	if err != nil {
		log.Fatal("Failed to subscribe to head events", err)
	}
	defer sub.Unsubscribe()

	// Start a goroutine to update the state from head notifications in the background
	update := make(chan *types.Header)

	go func() {
		for head := range update {
			// New chain head arrived, query the block and stream to clients
			timestamp := time.Unix(int64(head.Time), 0)
			if time.Since(timestamp) > time.Hour {
				fmt.Println("Skipping manager refresh, head too old", "number", head.Number, "hash", head.Hash(), "age", common.PrettyAge(timestamp))
				continue
			}

			block, err := m.ClientSlice[sliceIndex].BlockByNumber(context.Background(), head.Number[sliceIndex])
			if err != nil {
				log.Fatal("Failed to retrieve block from node", "err", err)
			}

			// Manager state retrieved, update locally and send to clients
			m.lock[sliceIndex].RLock()

			m.headers[sliceIndex] = head
			fmt.Println("Updated manager state", "number", head.Number, "hash", head.Hash(), "age", common.PrettyAge(timestamp))

			// Add block to cache

			for _, conn := range m.conns {
				if err := send(conn, map[string]interface{}{
					"block": block,
				}, time.Second); err != nil {
					fmt.Println("Failed to send block to client", "err", err)
					conn.conn.Close()
					continue
				}
				if err := send(conn, head, time.Second); err != nil {
					fmt.Println("Failed to send header to client", "err", err)
					conn.conn.Close()
				}
			}
			m.lock[sliceIndex].RUnlock()
		}
	}()
}

// sends transmits a data packet to the remote end of the websocket, but also
// setting a write deadline to prevent waiting forever on the node.
func send(conn *wsConn, value interface{}, timeout time.Duration) error {
	if timeout == 0 {
		timeout = 60 * time.Second
	}
	conn.wlock.Lock()
	defer conn.wlock.Unlock()
	conn.conn.SetWriteDeadline(time.Now().Add(timeout))
	return conn.conn.WriteJSON(value)
}
