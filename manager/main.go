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
	"github.com/dominant-strategies/go-quai/common"
	"github.com/dominant-strategies/go-quai/consensus/blake3pow"
	"github.com/dominant-strategies/go-quai/core/types"
	"github.com/dominant-strategies/go-quai/quaiclient/ethclient"
	"github.com/dominant-strategies/quai-manager/manager/util"
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
	engine *blake3pow.Blake3pow
	config util.Config

	header *types.Header

	sliceClients SliceClients
	lock         sync.Mutex
	location     []byte

	updatedCh chan *types.Header
	resultCh  chan *types.Header
	startCh   chan struct{}
	exitCh    chan struct{}
	doneCh    chan bool // channel for updating location

	previousNumber []*big.Int
}

type SliceClients struct {
	prime  *ethclient.Client
	region *ethclient.Client
	zone   *ethclient.Client
}

var exponentialBackoffCeilingSecs int64 = 14400 // 4 hours

func main() {
	// Load config
	config, err := util.LoadConfig("..")
	if err != nil {
		log.Fatal("cannot load config:", err)
	}

	// Parse mining location
	if len(os.Args) > 3 {
		raw := os.Args[1:3]
		region, _ := strconv.Atoi(raw[0])
		zone, _ := strconv.Atoi(raw[1])
		config.Location = common.Location{byte(region), byte(zone)}
	} else {
		log.Fatal("Not enough arguments supplied")
	}

	// Build manager config
	blake3Config := blake3pow.Config{
		NotifyFull: true,
	}
	blake3Engine := blake3pow.New(blake3Config, nil, false)
	m := &Manager{
		engine:         blake3Engine,
		sliceClients:   connectToSlice(config),
		header:         types.EmptyHeader(),
		resultCh:       make(chan *types.Header, resultQueueSize),
		updatedCh:      make(chan *types.Header, resultQueueSize),
		exitCh:         make(chan struct{}),
		startCh:        make(chan struct{}, 1),
		doneCh:         make(chan bool),
		location:       config.Location,
		previousNumber: make([]*big.Int, 3),
		config:         config,
	}
	m.previousNumber = []*big.Int{big.NewInt(0), big.NewInt(0), big.NewInt(0)}

	log.Println("Starting manager in location ", config.Location)

	m.fetchPendingHeader()

	go m.subscribePendingHeader()
	go m.resultLoop()
	go m.miningLoop()
	go m.SubmitHashRate()
	<-exit
}

// getNodeClients takes in a config and retrieves the Prime, Region, and Zone client
// that is used for mining in a slice.
func connectToSlice(config util.Config) SliceClients {
	var err error
	loc := config.Location
	clients := SliceClients{}

	primeConnected := false
	regionConnected := false
	zoneConnected := false
	for !primeConnected || !regionConnected || !zoneConnected {
		if config.PrimeURL != "" && !primeConnected {
			clients.prime, err = ethclient.Dial(config.PrimeURL)
			if err != nil {
				log.Println("Unable to connect to node:", "Prime", config.PrimeURL)
			} else {
				primeConnected = true
			}
		}
		if config.RegionURLs[loc.Region()] != "" && !regionConnected {
			clients.region, err = ethclient.Dial(config.RegionURLs[loc.Region()])
			if err != nil {
				log.Println("Unable to connect to node:", "Region", config.RegionURLs[loc.Region()])
			} else {
				regionConnected = true
			}
		}
		if config.ZoneURLs[loc.Region()][loc.Zone()] != "" && !zoneConnected {
			clients.zone, err = ethclient.Dial(config.ZoneURLs[loc.Region()][loc.Zone()])
			if err != nil {
				log.Println("Unable to connect to node:", "Zone", config.ZoneURLs[loc.Region()][loc.Zone()])
			} else {
				zoneConnected = true
			}
		}
	}

	return clients
}

// subscribePendingHeader subscribes to the head of the mining nodes in order to pass
// the most up to date block to the miner within the manager.
func (m *Manager) subscribePendingHeader() {
	// done channel in case best Location updates
	// subscribe to the pending block only if not synching
	// if checkSync == nil && err == nil {
	// Wait for chain events and push them to clients
	header := make(chan *types.Header)
	sub, err := m.sliceClients.zone.SubscribePendingHeader(context.Background(), header)
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
			return
		}
	}
	// }
}

// PendingBlocks gets the latest block when we have received a new pending header. This will get the receipts,
// transactions, and uncles to be stored during mining.
func (m *Manager) fetchPendingHeader() {
	var header *types.Header
	var err error

	m.lock.Lock()
	header, err = m.sliceClients.zone.GetPendingHeader(context.Background())

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

			header, err = m.sliceClients.zone.GetPendingHeader(context.Background())
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
	if header.Number(0) == nil {
		log.Println("Waiting to retrieve Prime header information...")
		return err
	}
	if header.Number(1) == nil {
		log.Println("Waiting to retrieve Region header information...")
		return err
	}
	if header.Number(2) == nil {
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
			// If the header has zero numbers
			if header.Number(0).Cmp(common.Big0) == 0 || header.Number(1).Cmp(common.Big0) == 0 || header.Number(2).Cmp(common.Big0) == 0 {
				continue
			}
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

			if !bytes.Equal(header.Location(), m.location) {
				continue
			}
			headerNull := m.headerNullCheck(header)
			if headerNull == nil {
				if header.Number(0).Cmp(m.previousNumber[0]) != 0 || header.Number(1).Cmp(m.previousNumber[1]) != 0 || header.Number(2).Cmp(m.previousNumber[2]) != 0 {
					if header.Number(0).Cmp(m.previousNumber[0]) != 0 {
						log.Println("Mining Block:  ", fmt.Sprintf("[%s %s %s]", color.Ize(color.Red, fmt.Sprint(header.Number(0))), header.Number(1).String(), header.Number(2).String()), "location", m.location, "difficulty", header.DifficultyArray())
					} else if header.Number(1).Cmp(m.previousNumber[1]) != 0 {
						log.Println("Mining Block:  ", fmt.Sprintf("[%s %s %s]", header.Number(0).String(), color.Ize(color.Yellow, fmt.Sprint(header.Number(1))), header.Number(2).String()), "location", m.location, "difficulty", header.DifficultyArray())
					} else {
						log.Println("Mining Block:  ", header.NumberArray(), "location", m.location, "difficulty", header.DifficultyArray())
					}
					m.previousNumber = header.NumberArray()
				}

				header.SetTime(uint64(time.Now().Unix()))
				if err := m.engine.Seal(header, m.resultCh, stopCh); err != nil {
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
	// id := crypto.Keccak256Hash(randomIdArray)

	var null float64 = 0
	go func() {
		for {
			select {
			case <-ticker.C:
				hashRate := m.engine.Hashrate()
				if hashRate != null {
					log.Println("Quai Miner  :   Hashes per second: ", hashRate)
					// m.engine.SubmitHashrate(hexutil.Uint64(hashRate), id)
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

			context, err := m.GetDifficultyOrder(header)
			if err != nil {
				log.Println("Block mined has an invalid order")
			}

			if context == 0 {
				log.Println(color.Ize(color.Red, "PRIME block :  "), header.NumberArray(), header.Hash())
			}

			if context == 1 {
				log.Println(color.Ize(color.Yellow, "REGION block:  "), header.NumberArray(), header.Hash())
			}

			if context == 2 {
				log.Println(color.Ize(color.Blue, "Zone block  :  "), header.NumberArray(), header.Hash())
			}

			// Check to see that all nodes are running before sending blocks to them.
			if !m.allChainsOnline() {
				log.Println("At least one of the chains is not online at the moment")
				continue
			}

			// Check proper difficulty for which nodes to send block to
			// Notify blocks to put in cache before assembling new block on node
			if context == 0 && header.Number(0) != nil {
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
			if context == 1 && header.Number(1) != nil {
				var wg sync.WaitGroup
				wg.Add(1)
				go m.SendMinedHeader(2, header, &wg)
				wg.Add(1)
				go m.SendMinedHeader(1, header, &wg)
				wg.Wait()
			}

			// If Zone difficulty send to Zone
			if context == 2 && header.Number(2) != nil {
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
	if !checkConnection(m.sliceClients.prime) {
		return false
	}
	if !checkConnection(m.sliceClients.region) {
		return false
	}
	if !checkConnection(m.sliceClients.zone) {
		return false
	}
	return true
}

// SendMinedHeader sends the mined block to its mining client with the transactions, uncles, and receipts.
func (m *Manager) SendMinedHeader(mined int, header *types.Header, wg *sync.WaitGroup) {
	if mined == 0 {
		m.sliceClients.prime.ReceiveMinedHeader(context.Background(), header)
	}
	if mined == 1 {
		m.sliceClients.region.ReceiveMinedHeader(context.Background(), header)
	}
	if mined == 2 {
		m.sliceClients.zone.ReceiveMinedHeader(context.Background(), header)
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

var (
	big2e256 = new(big.Int).Exp(big.NewInt(2), big.NewInt(256), big.NewInt(0)) // 2^256
)

// This function determines the difficulty order of a block
func (m *Manager) GetDifficultyOrder(header *types.Header) (int, error) {
	if header == nil {
		return common.HierarchyDepth, errors.New("no header provided")
	}
	blockhash := header.Hash()

	for i, difficulty := range header.DifficultyArray() {
		if difficulty != nil && big.NewInt(0).Cmp(difficulty) < 0 {
			target := new(big.Int).Div(big2e256, difficulty)
			if new(big.Int).SetBytes(blockhash.Bytes()).Cmp(target) <= 0 {
				return i, nil
			}
		}
	}
	return -1, errors.New("block does not satisfy minimum difficulty")
}
