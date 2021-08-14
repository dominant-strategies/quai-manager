package main

import (
	"context"
	"fmt"
	"log"

	"github.com/spruce-solutions/go-quai/ethclient"
	"github.com/spruce-solutions/quai-manager/manager/util"
)

func main() {
	config, err := util.LoadConfig("..")
	if err != nil {
		log.Fatal("cannot load config:", err)
	}
	fmt.Println(config)

	url := "http://127.0.0.1:8545"
	// Hello world, the web server
	client, err := ethclient.Dial(url)
	if err != nil {
		fmt.Println("Failed to dial, url: ", url, ", err: ", err)
		return
	}

	header, err := client.HeaderByNumber(context.Background(), nil)
	if err != nil {
		log.Fatal(err)
	}

	fmt.Println(header.Number)
}
