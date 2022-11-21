package main

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dominant-strategies/go-quai/common"
	"github.com/dominant-strategies/go-quai/core/types"
	"github.com/dominant-strategies/go-quai/crypto"
	"github.com/dominant-strategies/go-quai/params"
	"github.com/dominant-strategies/go-quai/quaiclient/ethclient"
	"github.com/dominant-strategies/quai-faucet/keystore"
	"github.com/spruce-solutions/go-quai/log"
)

var BASEFEE = common.Big1
var MINERTIP = common.Big1
var GAS = uint64(100000)
var VALUE = big.NewInt(params.Ether)
var PORT = "8547"

func TestOpETX(t *testing.T) {
	contract, err := hex.DecodeString("60806040526000806000806000600180620186a06706f05b59d3b20000735a457339697cb56e5a9bfa5267ea80d2c6375d986000f690505060698060446000396000f3fe6080604052600080fdfea2646970667358221220d51a551dccdf100829b6e928e928be7c4b803430100a5e618686c4ab3d17280964736f6c63782c302e382e31382d646576656c6f702e323032322e31312e372b636f6d6d69742e32636336363130652e6d6f64005d")
	if err != nil {
		t.Error(err.Error())
		t.Fail()
	}
	signer := types.LatestSigner(params.MainnetChainConfig)
	testKey, _ := crypto.HexToECDSA("b71c71a67e1177ad4e901695e1b4b9ee17ae16c6668d313eac2f96dbcda3f291") // Private Key
	addr := crypto.PubkeyToAddress(testKey.PublicKey)
	common.NodeLocation = *addr.Location() // Assuming we are in the same location as the provided key
	primeClient, err := ethclient.Dial("ws://127.0.0.1:" + PORT)
	if err != nil {
		t.Error(err.Error())
		t.Fail()
	}

	ks := keystore.NewKeyStore(filepath.Join(os.Getenv("HOME"), ".faucet", "keys"), keystore.StandardScryptN, keystore.StandardScryptP)
	// Load up the account keys and decrypt its password
	blob, err := ioutil.ReadFile(*accPassFlag)
	if err != nil {
		log.Crit("Failed to read account password contents", "file", *accPassFlag, "err", err)
	}
	pass := strings.TrimSuffix(string(blob), "\n")

	for i := 0; i < *numChainsFlag; i++ {
		if blob, err = ioutil.ReadFile("keystore/" + chainList[i] + ".json"); err != nil {
			log.Crit("Failed to read account key contents", "file", chainList[i]+".json", "err", err)
		}
		fmt.Println("Here", i)
		acc, err := ks.Import(blob, pass, pass)
		if err != nil && err != keystore.ErrAccountAlreadyExists {
			log.Crit("Failed to import faucet signer account", "err", err)
		}
		if err := ks.Unlock(acc, pass); err != nil {
			log.Crit("Failed to unlock faucet signer account", "err", err)
		}
	}

	i := uint8(0)
	nonce := uint64(0)
	contract = append(contract, i)
	for {
		contract[len(contract)-1] = i
		contractAddr := crypto.CreateAddress(addr, nonce, contract)
		if contractAddr.IsInChainScope() {
			break
		}
		i++
	}
	nonce, err = primeClient.NonceAt(context.Background(), addr, nil)
	if err != nil {
		t.Error(err.Error())
		t.Fail()
	}
	inner_tx := types.InternalTx{ChainID: params.MainnetChainConfig.ChainID, Nonce: nonce, GasTipCap: MINERTIP, GasFeeCap: BASEFEE, Gas: GAS, To: nil, Value: VALUE, Data: contract}
	tx, err := types.SignTx(types.NewTx(&inner_tx), signer, testKey)
	if err != nil {
		t.Error(err.Error())
		t.Fail()
	}

	err = primeClient.SendTransaction(context.Background(), tx)
	if err != nil {
		t.Error(err.Error())
		t.Fail()
	}

}
