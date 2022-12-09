package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"path/filepath"
	"testing"

	"github.com/dominant-strategies/go-quai/common"
	"github.com/dominant-strategies/go-quai/core/types"
	"github.com/dominant-strategies/go-quai/crypto"
	"github.com/dominant-strategies/go-quai/params"
	"github.com/dominant-strategies/go-quai/quaiclient/ethclient"
	accounts "github.com/dominant-strategies/quai-accounts"
	"github.com/dominant-strategies/quai-accounts/keystore"
	"github.com/holiman/uint256"
	"github.com/spruce-solutions/quai-manager/manager/util"
)

var BASEFEE = big.NewInt(2 * params.GWei)
var MINERTIP = big.NewInt(2 * params.GWei)
var GAS = uint64(110000)
var VALUE = big.NewInt(1111111111111111)
var PARAMS = params.RopstenChainConfig
var numChains = 13
var chainList = []string{"prime", "cyprus", "cyprus1", "cyprus2", "cyprus3", "paxos", "paxos1", "paxos2", "paxos3", "hydra", "hydra1", "hydra2", "hydra3"}
var from_zone = 0

func TestOpETX(t *testing.T) {
	config, err := util.LoadConfig(".")
	if err != nil {
		t.Error("cannot load config: " + err.Error())
		t.Fail()
	}
	allClients := getNodeClients(config)

	contract, err := hex.DecodeString("60806040526040516101e03803806101e0833981810160405281019061002591906100dc565b600080600080600086888a8c8e6000f6905050505050505050610169565b600080fd5b600073ffffffffffffffffffffffffffffffffffffffff82169050919050565b600061007382610048565b9050919050565b61008381610068565b811461008e57600080fd5b50565b6000815190506100a08161007a565b92915050565b6000819050919050565b6100b9816100a6565b81146100c457600080fd5b50565b6000815190506100d6816100b0565b92915050565b60008060008060008060c087890312156100f9576100f8610043565b5b600061010789828a01610091565b965050602061011889828a016100c7565b955050604061012989828a016100c7565b945050606061013a89828a016100c7565b935050608061014b89828a016100c7565b92505060a061015c89828a016100c7565b9150509295509295509295565b6069806101776000396000f3fe6080604052600080fdfea2646970667358221220ae701927ce1c6a30dbd24ad1b79952d125849aa7fd08aaa826c17c489699f20764736f6c63782c302e382e31382d646576656c6f702e323032322e31312e382b636f6d6d69742e36306161353861362e6d6f64005d")
	if err != nil {
		t.Error(err.Error())
		t.Fail()
	}

	ks := keystore.NewKeyStore(filepath.Join(os.Getenv("HOME"), ".faucet", "keys"), keystore.StandardScryptN, keystore.StandardScryptP)
	pass := ""

	for i := 0; i < numChains; i++ {
		blob, err := ioutil.ReadFile("keystore/" + chainList[i] + ".json") // put keystore folder in the current directory
		if err != nil {
			t.Error("Failed to read account key contents", "file", chainList[i]+".json", "err", err)
			t.Fail()
		}
		acc, err := ks.Import(blob, pass, pass)
		if err != nil && err != keystore.ErrAccountAlreadyExists {
			t.Error("Failed to import faucet signer account", "err", err)
			t.Fail()
		}
		if err := ks.Unlock(acc, pass); err != nil {
			t.Error("Failed to unlock faucet signer account", "err", err)
			t.Fail()
		}
		addAccToClient(&allClients, acc, i)
	}

	for i := 0; i < 1; i++ {
		if !allClients.zonesAvailable[from_zone][i] {
			continue
		}
		client := allClients.zoneClients[from_zone][i]
		from := allClients.zoneAccounts[from_zone][i]
		common.NodeLocation = *from.Address.Location() // Assuming we are in the same location as the provided key

		toAddr := allClients.zoneAccounts[from_zone+1][i].Address
		toAddr[len(toAddr)-1] += 1 // Tweak the recipient
		to, err := uint256.FromHex(toAddr.Hex())
		if err != nil {
			t.Error(err.Error())
			t.Fail()
		}
		amount, _ := uint256.FromBig(VALUE)
		limit := uint256.NewInt(GAS)
		tip, _ := uint256.FromBig(MINERTIP)
		baseFee, _ := uint256.FromBig(BASEFEE)
		data := make([]byte, 0, 0)
		temp := to.Bytes32()
		data = append(data, temp[:]...)
		temp = amount.Bytes32()
		data = append(data, temp[:]...)
		temp = limit.Bytes32()
		data = append(data, temp[:]...)
		temp = tip.Bytes32()
		data = append(data, temp[:]...)
		temp = baseFee.Bytes32()
		data = append(data, temp[:]...)
		contract = append(contract, data...)

		nonce, err := client.NonceAt(context.Background(), from.Address, nil)
		if err != nil {
			t.Error(err.Error())
			t.Fail()
		}
		i := uint8(0)
		temp = uint256.NewInt(uint64(i)).Bytes32()
		contract = append(contract, temp[:]...)
		for {
			contract[len(contract)-1] = i // one byte (8 bits) for contract nonce is sufficient
			contractAddr := crypto.CreateAddress(from.Address, nonce, contract)
			if contractAddr.IsInChainScope() {
				break
			}
			i++
		}
		accessList := types.AccessList{}
		tmpVal := big.NewInt(0).Add(VALUE, big.NewInt(1e17))
		inner_tx := types.InternalTx{ChainID: PARAMS.ChainID, Nonce: nonce, GasTipCap: MINERTIP, GasFeeCap: BASEFEE, Gas: GAS, To: nil, Value: tmpVal, Data: contract, AccessList: accessList}
		tx, err := ks.SignTx(from, types.NewTx(&inner_tx), PARAMS.ChainID)
		if err != nil {
			t.Error(err.Error())
			t.Fail()
		}

		err = client.SendTransaction(context.Background(), tx)
		if err != nil {
			t.Error(err.Error())
			t.Fail()
		}
	}

}

// Block struct to hold all Client fields.
type orderedBlockClients struct {
	primeClient      *ethclient.Client
	primeAvailable   bool
	primeAccount     accounts.Account
	regionClients    []*ethclient.Client
	regionsAvailable []bool
	regionAccounts   []accounts.Account
	zoneClients      [][]*ethclient.Client
	zonesAvailable   [][]bool
	zoneAccounts     [][]accounts.Account
}

// getNodeClients takes in a config and retrieves the Prime, Region, and Zone client
// that is used for mining in a slice.
func getNodeClients(config util.Config) orderedBlockClients {

	// initializing all the clients
	allClients := orderedBlockClients{
		primeAvailable:   false,
		regionClients:    make([]*ethclient.Client, 3),
		regionsAvailable: make([]bool, 3),
		regionAccounts:   make([]accounts.Account, 3),
		zoneClients:      make([][]*ethclient.Client, 3),
		zonesAvailable:   make([][]bool, 3),
		zoneAccounts:     make([][]accounts.Account, 3),
	}

	for i := range allClients.zoneClients {
		allClients.zoneClients[i] = make([]*ethclient.Client, 3)
	}
	for i := range allClients.zonesAvailable {
		allClients.zonesAvailable[i] = make([]bool, 3)
	}
	for i := range allClients.zoneClients {
		allClients.zoneAccounts[i] = make([]accounts.Account, 3)
	}

	// add Prime to orderedBlockClient array at [0]
	if config.PrimeURL != "" {
		primeClient, err := ethclient.Dial(config.PrimeURL)
		if err != nil {
			fmt.Println("Unable to connect to node:", "Prime", config.PrimeURL)
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
				fmt.Println("Unable to connect to node:", "Region", i+1, regionURL)
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
					fmt.Println("Unable to connect to node:", "Zone", i+1, j+1, zoneURL)
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

func addAccToClient(clients *orderedBlockClients, acc accounts.Account, i int) {
	switch i {
	case 0:
		clients.primeAccount = acc
	case 1:
		clients.regionAccounts[0] = acc
	case 2:
		clients.zoneAccounts[0][0] = acc
	case 3:
		clients.zoneAccounts[0][1] = acc
	case 4:
		clients.zoneAccounts[0][2] = acc
	case 5:
		clients.regionAccounts[1] = acc
	case 6:
		clients.zoneAccounts[1][0] = acc
	case 7:
		clients.zoneAccounts[1][1] = acc
	case 8:
		clients.zoneAccounts[1][2] = acc
	case 9:
		clients.regionAccounts[2] = acc
	case 10:
		clients.zoneAccounts[2][0] = acc
	case 11:
		clients.zoneAccounts[2][1] = acc
	case 12:
		clients.zoneAccounts[2][2] = acc
	default:
		fmt.Println("Error adding account to client, chain not found " + fmt.Sprint(i))
	}
}
