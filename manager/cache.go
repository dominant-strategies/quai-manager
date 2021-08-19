
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
			m.blocks[sliceIndex] = block
			fmt.Println("Updated manager state", "number", head.Number, "hash", head.Hash(), "age", common.PrettyAge(timestamp))
			m.lock[sliceIndex].RUnlock()
			m.fetchPendingBlocks()
		}
	}()
	// Wait for various events and assing to the appropriate background threads
	for {
		select {
		case head := <-heads:
			// New head arrived, send if for state update if there's none running
			select {
			case update <- head:
			default:
			}
		}
	}
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